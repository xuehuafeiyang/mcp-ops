#!/usr/bin/env python3
"""把 docs/MANUAL.md 渲染为可独立分发的 HTML 与 PDF。

用法：
    python3 scripts/build-doc.py            # 生成 docs/MANUAL.html 与 docs/MANUAL.pdf
依赖：markdown、weasyprint；PDF 需要系统中文字体（Noto CJK）。
"""
import pathlib
import sys

import markdown
from weasyprint import HTML

ROOT = pathlib.Path(__file__).resolve().parent.parent
SRC = ROOT / "docs" / "MANUAL.md"
HTML_OUT = ROOT / "docs" / "MANUAL.html"
PDF_OUT = ROOT / "docs" / "MANUAL.pdf"

CSS = """
@page {
  size: A4;
  margin: 20mm 18mm 18mm 18mm;
  @bottom-center {
    content: "mcp-ops 使用手册 v0.5.1  ·  第 " counter(page) " / " counter(pages) " 页";
    font-size: 8.5pt; color: #888;
    font-family: "Noto Sans CJK SC", sans-serif;
  }
}
body {
  font-family: "Noto Serif CJK SC", "Songti SC", serif;
  font-size: 10.5pt; line-height: 1.75; color: #1a1a1a;
  max-width: 900px; margin: 0 auto; padding: 24px;
}
h1 { font-size: 24pt; color: #0b3d6b; border-bottom: 3px solid #0b3d6b;
     padding-bottom: 8px; margin-top: 0; }
h2 { font-size: 15pt; color: #0b3d6b; margin-top: 26px;
     border-left: 4px solid #2f7fc1; padding-left: 10px; }
h3 { font-size: 12pt; color: #17466f; margin-top: 20px; }
h4 { font-size: 10.5pt; color: #333; }
code { font-family: "DejaVu Sans Mono", "Noto Sans Mono CJK SC", monospace;
       font-size: 9pt; background: #f2f4f7; padding: 1px 4px; border-radius: 3px; }
pre { background: #f7f8fa; border: 1px solid #e1e5ea; border-radius: 5px;
      padding: 10px 12px; overflow-x: auto; line-height: 1.55;
      page-break-inside: avoid; }
pre code { background: none; padding: 0; font-size: 8.6pt; }
table { border-collapse: collapse; width: 100%; margin: 12px 0;
        font-size: 9.4pt; page-break-inside: avoid; }
th { background: #eaf1f8; color: #0b3d6b; font-weight: 600; }
th, td { border: 1px solid #d6dde5; padding: 6px 8px; vertical-align: top; }
th:first-child, td:first-child { min-width: 4.8em; }
tr:nth-child(even) td { background: #fafbfc; }
blockquote { border-left: 4px solid #c9d6e3; margin-left: 0; padding: 2px 14px;
             color: #45505b; background: #f8fafc; }
a { color: #1f6fb2; text-decoration: none; }
hr { border: none; border-top: 1px solid #dde3ea; margin: 26px 0; }
ul, ol { padding-left: 22px; }
li { margin: 3px 0; }
"""


def main() -> int:
    md_text = SRC.read_text(encoding="utf-8")
    body = markdown.markdown(
        md_text,
        extensions=["tables", "fenced_code", "toc", "sane_lists", "attr_list"],
        extension_configs={"toc": {"permalink": False}},
    )
    page = f"""<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>mcp-ops 使用手册 v0.5.1</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>{CSS}</style></head>
<body>{body}</body></html>"""
    HTML_OUT.write_text(page, encoding="utf-8")
    print(f"HTML -> {HTML_OUT}")

    HTML(string=page, base_url=str(ROOT)).write_pdf(str(PDF_OUT))
    size = PDF_OUT.stat().st_size / 1024
    print(f"PDF  -> {PDF_OUT} ({size:.0f} KB)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
