// linenums.js — line anchors in blob views: highlight + scroll #L<n> or a
// #L<a>:<b> range, and click/drag on the number column to select lines.
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
// returns the span.lnt directly. Number cells and code lines are parallel
// arrays: nums[i] is the number for lines[i].
(function () {
  "use strict";

  // parseHash returns {a, b} (1-based, a <= b) or null. Canonical form is
  // #L5 / #L5:9; #L5:L9, #L5-9, and #L5-L9 are accepted leniently.
  function parseHash(h) {
    var m = /^#L(\d+)(?:[:-]L?(\d+))?$/.exec(h || "");
    if (!m) return null;
    var a = parseInt(m[1], 10);
    var b = m[2] ? parseInt(m[2], 10) : a;
    if (!a || !b) return null;
    return a <= b ? { a: a, b: b } : { a: b, b: a };
  }

  function fmtHash(a, b) {
    return a === b ? "#L" + a : "#L" + a + ":" + b;
  }

  function clear() {
    document.querySelectorAll(".chroma .hl").forEach(function (el) {
      el.classList.remove("hl");
    });
  }

  // markRange highlights number cells La..Lb plus their code lines and
  // returns the first number cell (or null if La doesn't exist).
  function markRange(a, b) {
    clear();
    var first = document.getElementById("L" + a);
    if (!first) return null;
    var chroma = first.closest(".chroma");
    var nums = chroma ? chroma.querySelectorAll(".lnt") : [];
    var lines = chroma ? chroma.querySelectorAll(".line") : [];
    var startIdx = Array.prototype.indexOf.call(nums, first);
    for (var n = a; n <= b; n++) {
      var lnt = document.getElementById("L" + n);
      if (!lnt) break; // past end of file
      lnt.classList.add("hl");
      var idx = startIdx + (n - a);
      if (startIdx >= 0 && idx < lines.length) lines[idx].classList.add("hl");
    }
    return first;
  }

  function apply(scroll) {
    var r = parseHash(location.hash);
    if (!r) {
      clear();
      return;
    }
    var first = markRange(r.a, r.b);
    if (first && scroll && first.scrollIntoView) {
      first.scrollIntoView({ block: "center" });
    }
  }

  // lineOf maps an event target inside a number cell to its line number
  // (0 when the target is not in a number cell).
  function lineOf(el) {
    var lnt = el && el.closest ? el.closest(".lnt") : null;
    if (!lnt) return 0;
    var m = /^L(\d+)$/.exec(lnt.id || "");
    return m ? parseInt(m[1], 10) : 0;
  }

  // Drag selection: mousedown on a number cell anchors the selection,
  // dragging over other cells live-previews the range, mouseup commits it
  // to location.hash. suppressScroll stops the resulting hashchange from
  // re-centering a selection the user just made by hand.
  var dragFrom = 0; // 0 = no drag in progress
  var suppressScroll = false;

  document.addEventListener("mousedown", function (e) {
    if (e.button !== 0) return;
    var n = lineOf(e.target);
    if (!n) return;
    dragFrom = n;
    markRange(n, n);
    e.preventDefault(); // keep the drag from starting a text selection
  });

  document.addEventListener("mouseover", function (e) {
    if (!dragFrom) return;
    var n = lineOf(e.target);
    if (!n) return;
    markRange(Math.min(dragFrom, n), Math.max(dragFrom, n));
  });

  document.addEventListener("mouseup", function (e) {
    if (!dragFrom) return;
    var from = dragFrom;
    dragFrom = 0;
    var n = lineOf(e.target) || from; // released off-column: keep single line
    var h = fmtHash(Math.min(from, n), Math.max(from, n));
    if (location.hash !== h) {
      suppressScroll = true;
      location.hash = h; // hashchange handler re-applies the highlight
    }
  });

  // mouseup already set the hash; letting the <a href="#Ln"> click through
  // would make the browser re-scroll to the anchor. Keyboard activation
  // (detail === 0) keeps the native behavior so Enter still works.
  document.addEventListener("click", function (e) {
    if (!lineOf(e.target)) return;
    if (e.detail === 0) return;
    e.preventDefault();
  });

  window.addEventListener("hashchange", function () {
    var scroll = !suppressScroll;
    suppressScroll = false;
    apply(scroll);
  });
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { apply(true); });
  } else {
    apply(true);
  }
})();
