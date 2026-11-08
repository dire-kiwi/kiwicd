import { Link } from "react-router-dom";
import { useMe } from "../App";
import { describeAccess, isAdmin } from "../access";
import { APITokens } from "../components/APITokens";
import { GitHubAppSettings } from "../components/GitHubApp";
import { SettingsTable } from "../components/SettingsTable";
import { useDocumentTitle } from "../hooks";

export function SettingsPage() {
  useDocumentTitle("Settings");
  const me = useMe();
  const admin = isAdmin(me);
  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1>Settings</h1>
          {admin ? (
            <p className="muted">
              Global secrets and variables, available to workflows in every repository, the GitHub App and who may sign in with
              it, and API tokens. Changes are recorded in the <Link to="/audit">audit log</Link>.
            </p>
          ) : (
            <p className="muted">
              You are {me ? describeAccess(me.access) : "signed in"}. An admin manages the server's own settings and roles.
            </p>
          )}
        </div>
      </div>
      <div className="stack">
        {admin && <SettingsTable kind="secrets" />}
        {admin && <SettingsTable kind="variables" />}
        {admin && <GitHubAppSettings />}
        <APITokens />
      </div>
    </div>
  );
}
