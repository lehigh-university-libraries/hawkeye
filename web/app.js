"use strict";

let refreshing = false;
setInterval(async () => {
  if (refreshing || document.hidden || !document.querySelector("#auto-refresh").checked || window.getSelection().toString()) return;
  refreshing = true;
  try {
    const response = await fetch(location.href, {cache: "no-store"});
    if (!response.ok) throw new Error("Refresh failed");
    const page = new DOMParser().parseFromString(await response.text(), "text/html");
    const content = page.querySelector("#content");
    if (!content) throw new Error("No report view");
    // A selection may have started while the request was in flight.
    if (!window.getSelection().toString() && document.querySelector("#auto-refresh").checked) {
      document.querySelector("#content").replaceWith(content);
      document.querySelector("#refresh-status").textContent = "Updated " + new Date().toLocaleTimeString();
    }
  } catch {
    document.querySelector("#refresh-status").textContent = "Refresh failed; showing the previous saved view.";
  } finally {
    refreshing = false;
  }
}, 10000);

document.addEventListener("click", async event => {
  if (event.target.id !== "copy-table") return;
  document.querySelector("#auto-refresh").checked = false;
  const table = document.querySelector("#results");
  const status = document.querySelector("#copy-status");
  const text = Array.from(table.rows, row => Array.from(row.cells, cell => cell.textContent.replace(/[\t\r\n]+/g, " ").trim()).join("\t")).join("\n");
  try {
    if (!navigator.clipboard || !window.ClipboardItem) throw new Error("Use selection");
    await navigator.clipboard.write([new ClipboardItem({
      "text/html": new Blob([table.outerHTML], {type: "text/html"}),
      "text/plain": new Blob([text], {type: "text/plain"})
    })]);
    status.textContent = "Copied. Paste into Google Sheets. Refresh is paused.";
  } catch {
    const selection = window.getSelection();
    const range = document.createRange();
    range.selectNode(table);
    selection.removeAllRanges();
    selection.addRange(range);
    const copied = document.execCommand("copy");
    status.textContent = copied ? "Copied. Paste into Google Sheets. Refresh is paused." : "Table selected. Press Ctrl/Cmd+C, then paste into Sheets.";
  }
});
