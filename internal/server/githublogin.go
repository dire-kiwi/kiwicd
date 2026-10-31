package server

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Signing in with GitHub uses the GitHub App's OAuth client (see github.go):
// /api/auth/github sends the browser to GitHub, which sends it back to
// /api/auth/github/callback with a code. kiwicd exchanges that for a token,
// asks GitHub who the user is and lets them in if they have a role (see
// access.go): their login has one, or they're a member of an organization or
// team with one that the app is installed on. The token isn't kept, so
// membership is checked when someone signs in: GitHub sessions last a week
// rather than the admin's 30 days.

const (
	githubStateCookie = "kiwicd_github_state"
	githubSessionTime = 7 * 24 * time.Hour
)

func (s *Server) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	a, err := s.githubApp()
	writeJSON(w, map[string]bool{"github": err == nil && a.ClientID != ""})
}

func loginRedirect(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/login?"+url.Values{"error": {msg}}.Encode(), http.StatusSeeOther)
}

func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	a, err := s.githubApp()
	if err != nil {
		loginRedirect(w, r, "Signing in with GitHub isn't set up.")
		return
	}
	state := randomToken(16)
	base := s.publicURL(r)
	http.SetCookie(w, &http.Cookie{
		Name: githubStateCookie, Value: state, Path: "/api/auth/github", MaxAge: 600, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(base, "https://"),
	})
	q := url.Values{
		"client_id":    {a.ClientID},
		"redirect_uri": {base + "/api/auth/github/callback"},
		"state":        {state},
		"allow_signup": {"false"},
	}
	http.Redirect(w, r, s.cfg.GitHubURL+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleGitHubLoginCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := r.Cookie(githubStateCookie)
	http.SetCookie(w, &http.Cookie{Name: githubStateCookie, Value: "", Path: "/api/auth/github", MaxAge: -1})
	// The state ties the callback to a sign-in this browser started, so
	// nobody can sign someone in as themselves with a link.
	if err != nil || c.Value == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(q.Get("state"))) != 1 {
		loginRedirect(w, r, "The sign-in with GitHub expired or didn't start here; try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		loginRedirect(w, r, "GitHub: "+firstNonEmpty(q.Get("error_description"), e))
		return
	}
	a, err := s.githubApp()
	if err != nil {
		loginRedirect(w, r, "Signing in with GitHub isn't set up.")
		return
	}
	login, err := s.githubUser(r, a, q.Get("code"))
	if err != nil {
		s.log.Warn("GitHub sign-in", "err", err)
		loginRedirect(w, r, "Signing in with GitHub failed: "+err.Error())
		return
	}
	allowed, err := s.checkMemberships(r.Context(), a, login)
	if err != nil {
		loginRedirect(w, r, err.Error())
		return
	}
	if !allowed {
		s.log.Warn("GitHub sign-in refused", "login", login, "client", s.clientIP(r))
		loginRedirect(w, r, "GitHub user "+login+" may not sign in to this kiwicd.")
		return
	}
	if err := s.startSession(w, r, login, githubSessionTime); err != nil {
		loginRedirect(w, r, err.Error())
		return
	}
	s.log.Info("signed in with GitHub", "login", login, "client", s.clientIP(r))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// githubUser exchanges an OAuth code for a token and returns the login of
// the user it's for.
func (s *Server) githubUser(r *http.Request, a *githubApp, code string) (string, error) {
	if code == "" {
		return "", errors.New("no code")
	}
	form := url.Values{
		"client_id":     {a.ClientID},
		"client_secret": {a.ClientSecret},
		"code":          {code},
		"redirect_uri":  {s.publicURL(r) + "/api/auth/github/callback"},
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", s.cfg.GitHubURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kiwicd")
	var tok struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := s.githubDo(req, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New(firstNonEmpty(tok.ErrorDescription, tok.Error, "GitHub gave no token"))
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := s.githubAPI(r.Context(), "GET", "/user", "Bearer "+tok.AccessToken, nil, &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", errors.New("GitHub didn't say who you are")
	}
	return user.Login, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
