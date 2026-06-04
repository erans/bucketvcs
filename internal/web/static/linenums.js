// linenums.js — highlight + scroll the #L<n> target line in blob views.
// CSP-friendly (no inline script); loaded with defer from base.html.
//
// Chroma v2 table-mode structure (WithLinkableLineNumbers + LineNumbersInTable):
//   div.chroma
//     table.lntable > tr
//       td.lntd   (numbers column)
//         pre > span.lnt[id="L1"] > a.lnlinks[href="#L1"]
//       td.lntd   (code column)
//         pre > code > span.line > span.cl > ...
//
// id="L<n>" is on span.lnt, NOT on the <a>; document.getElementById("L1")
// returns the span.lnt directly.
(function () {
  "use strict";
  function clear() {
    document.querySelectorAll(".chroma .hl").forEach(function (el) {
      el.classList.remove("hl");
    });
  }
  function apply(scroll) {
    clear();
    var m = /^#(L(\d+))$/.exec(location.hash || "");
    if (!m) return;
    // getElementById returns span.lnt (id is on span.lnt, not on <a>).
    var lnt = document.getElementById(m[1]);
    if (!lnt) return;
    lnt.classList.add("hl");
    // Find the code line at the same index as this number cell.
    var chroma = lnt.closest(".chroma");
    if (chroma) {
      var nums = chroma.querySelectorAll(".lnt");
      var lines = chroma.querySelectorAll(".line");
      var idx = Array.prototype.indexOf.call(nums, lnt);
      if (idx >= 0 && idx < lines.length) {
        lines[idx].classList.add("hl");
      }
    }
    if (scroll && lnt.scrollIntoView) lnt.scrollIntoView({ block: "center" });
  }
  window.addEventListener("hashchange", function () { apply(true); });
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { apply(true); });
  } else {
    apply(true);
  }
})();
