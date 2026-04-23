// Injects "Show Markdown" / "Copy Markdown" buttons above the right-rail
// table of contents so readers can grab the raw source for LLMs.
(function () {
  "use strict";

  function rawMarkdownUrl() {
    var path = window.location.pathname;
    if (path.endsWith("/")) return path + "index.md";
    if (path.endsWith(".html")) return path.slice(0, -5) + ".md";
    return path + ".md";
  }

  async function fetchMarkdown() {
    var resp = await fetch(rawMarkdownUrl(), { cache: "no-cache" });
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    return resp.text();
  }

  function flash(btn, state) {
    // Avoid layout shift: keep the label fixed, toggle a color-only state class.
    btn.classList.remove(
      "llm-actions__btn--success",
      "llm-actions__btn--error"
    );
    btn.classList.add("llm-actions__btn--" + state);
    clearTimeout(btn._flashTimer);
    btn._flashTimer = setTimeout(function () {
      btn.classList.remove("llm-actions__btn--" + state);
    }, 1200);
  }

  async function onCopy(ev) {
    var btn = ev.currentTarget;
    try {
      var text = await fetchMarkdown();
      await navigator.clipboard.writeText(text);
      flash(btn, "success");
    } catch (e) {
      console.error("Copy Markdown failed:", e);
      flash(btn, "error");
    }
  }

  async function onShow(ev) {
    var btn = ev.currentTarget;
    try {
      var text = await fetchMarkdown();
      // Render inline as plain text instead of navigating to the .md URL
      // directly — most servers send it with a download disposition.
      var blob = new Blob([text], { type: "text/plain; charset=utf-8" });
      var url = URL.createObjectURL(blob);
      window.open(url, "_blank", "noopener");
      // Revoke a bit later so the new tab has time to load the blob.
      setTimeout(function () {
        URL.revokeObjectURL(url);
      }, 30000);
    } catch (e) {
      console.error("Show Markdown failed:", e);
      flash(btn, "error");
    }
  }

  function inject() {
    var sidebar = document.querySelector(
      ".md-sidebar--secondary .md-sidebar__scrollwrap"
    );
    if (!sidebar) return;
    if (sidebar.querySelector(".llm-actions")) return;

    var wrap = document.createElement("div");
    wrap.className = "llm-actions";
    wrap.setAttribute("aria-label", "Page source");

    var show = document.createElement("button");
    show.type = "button";
    show.className = "llm-actions__btn";
    show.textContent = "Show Markdown";
    show.title = "View the raw Markdown source for this page";
    show.addEventListener("click", onShow);

    var copy = document.createElement("button");
    copy.type = "button";
    copy.className = "llm-actions__btn";
    copy.textContent = "Copy Markdown";
    copy.title = "Copy the raw Markdown source to your clipboard";
    copy.addEventListener("click", onCopy);

    wrap.appendChild(show);
    wrap.appendChild(copy);

    sidebar.insertBefore(wrap, sidebar.firstChild);
  }

  if (typeof document$ !== "undefined" && document$.subscribe) {
    document$.subscribe(inject);
  } else if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", inject);
  } else {
    inject();
  }
})();
