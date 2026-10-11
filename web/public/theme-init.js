// Applies the saved theme before first paint. Loaded as a blocking script from index.html so a
// strict script-src ('self') CSP needs no inline hash. Keep the theme list in sync with
// src/shell/themes.ts; the default theme (eighties) is the :root block and needs no attribute.
try {
  var theme = localStorage.getItem('mainloop-theme');
  if (['dracula', 'nord', 'gruvbox', 'tokyo-night'].indexOf(theme) !== -1) {
    document.documentElement.setAttribute('data-theme', theme);
  }
} catch (e) {
  // Storage can be blocked; the default theme applies.
}
