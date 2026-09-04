// The share page's one interaction: copy the link to the clipboard.
// Served from /static/ with Cache-Control: no-cache. This file is intentionally
// tiny and stable — if it ever changes, bump the ?v= on its <script> tag.
(function () {
  var btn = document.getElementById("copy");
  if (!btn) return;
  btn.addEventListener("click", function () {
    var url = btn.getAttribute("data-url");
    var done = function () {
      var was = btn.textContent;
      btn.textContent = "Copied";
      btn.classList.add("copied");
      setTimeout(function () {
        btn.textContent = was;
        btn.classList.remove("copied");
      }, 1400);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(url).then(done, function () { window.prompt("Copy this link:", url); });
    } else {
      window.prompt("Copy this link:", url);
    }
  });
})();
