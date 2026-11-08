import DOMPurify from "dompurify";
import { marked } from "marked";
import { memo, useMemo } from "react";

// Step summaries are written by workflows, including a pull request's, so
// they are untrusted: the HTML marked makes (and any HTML in the Markdown) is
// sanitized. Beyond DOMPurify's defaults, nothing may restyle or overlay the
// page (no style or class attributes, no <style>), there are no forms,
// frames or media, and ids are prefixed so they can't clobber the page's.
const purify = DOMPurify(window);

const config = {
  USE_PROFILES: { html: true },
  FORBID_TAGS: ["style", "form", "button", "textarea", "select", "option", "iframe", "frame", "object", "embed", "video", "audio", "source", "track", "picture", "link", "meta", "base"],
  FORBID_ATTR: ["style", "class", "srcset", "action", "formaction"],
  SANITIZE_NAMED_PROPS: true,
};

/** renderMarkdown turns untrusted GitHub-flavored Markdown into safe HTML. */
export function renderMarkdown(src: string): string {
  const html = marked.parse(src, { async: false, gfm: true }) as string;
  const frag = purify.sanitize(html, { ...config, RETURN_DOM_FRAGMENT: true });
  // The page's Content-Security-Policy blocks other sites' images, and a
  // relative one would fetch from kiwicd as the viewer: show those as links,
  // keeping only data: images.
  frag.querySelectorAll("img").forEach((img) => {
    const src = img.getAttribute("src") ?? "";
    if (/^data:image\/(png|gif|jpeg|webp);/i.test(src)) return;
    const alt = img.getAttribute("alt") || "image";
    if (/^data:/i.test(src)) {
      img.replaceWith(alt);
      return;
    }
    const a = document.createElement("a");
    a.setAttribute("href", src);
    a.textContent = alt;
    img.replaceWith(a);
  });
  // Task-list checkboxes are the only inputs, and read-only.
  frag.querySelectorAll("input").forEach((input) => {
    if (input.getAttribute("type") !== "checkbox") input.remove();
    else input.setAttribute("disabled", "");
  });
  frag.querySelectorAll("a[href]").forEach((a) => {
    a.setAttribute("target", "_blank");
    a.setAttribute("rel", "noopener noreferrer nofollow");
  });
  const div = document.createElement("div");
  div.appendChild(frag);
  return div.innerHTML;
}

export const Markdown = memo(function Markdown({ source }: { source: string }) {
  const html = useMemo(() => renderMarkdown(source), [source]);
  return <div className="markdown" dangerouslySetInnerHTML={{ __html: html }} />;
});
