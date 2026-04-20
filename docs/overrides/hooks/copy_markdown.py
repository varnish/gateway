"""Copy each page's source Markdown into the built site.

Emits `<url>index.md` alongside `<url>index.html` so the docs site can expose
a "Show Markdown" / "Copy Markdown" affordance backed by a real URL that users
can fetch or feed to an LLM.
"""

from __future__ import annotations

import shutil
from pathlib import Path

_pages: list[tuple[str, str]] = []


def on_files(files, config, **kwargs):
    _pages.clear()
    for f in files:
        if f.is_documentation_page():
            _pages.append((f.src_path, f.url))
    return files


def on_post_build(config, **kwargs) -> None:
    site_dir = Path(config["site_dir"])
    docs_dir = Path(config["docs_dir"])

    for src_path, url in _pages:
        # url is '' for root, 'foo/bar/' for nested pages when
        # use_directory_urls is true (the MkDocs default).
        dest_dir = site_dir / url
        dest_dir.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(docs_dir / src_path, dest_dir / "index.md")
