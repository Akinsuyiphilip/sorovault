// Copy buttons for the values people paste into other tools: contract IDs,
// WASM hashes and function signatures.
//
// A button names the value it copies with data-copy, which is a selector
// for the element holding it. The buttons are hidden by default and
// revealed by marking the document, so a page without JavaScript shows the
// values as plain, selectable text instead.
(function () {
  "use strict";

  var SELECTOR = "button.copy[data-copy]";

  // The document is marked before anything is painted, so the buttons
  // never flash into view.
  document.documentElement.classList.add("js");

  function buttons() {
    return document.querySelectorAll(SELECTOR);
  }

  // value returns the text a button copies: the contents of the element it
  // names, with the whitespace a template introduced squeezed out.
  function value(button) {
    var element = document.querySelector(button.getAttribute("data-copy"));
    if (!element) {
      return "";
    }
    return element.textContent.replace(/\s+/g, " ").trim();
  }

  // select picks the text out, for the browsers without a clipboard API.
  function select(button) {
    var element = document.querySelector(button.getAttribute("data-copy"));
    if (!element) {
      return false;
    }
    var range = document.createRange();
    range.selectNodeContents(element);
    var selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    return true;
  }

  // status returns the live region, creating it the first time it is
  // needed.
  function status() {
    var region = document.getElementById("copy-status");
    if (!region) {
      region = document.createElement("div");
      region.id = "copy-status";
      region.className = "visually-hidden";
      region.setAttribute("role", "status");
      region.setAttribute("aria-live", "polite");
      document.body.appendChild(region);
    }
    return region;
  }

  // announce says something in the live region, so a screen reader hears
  // that the copy worked.
  function announce(message) {
    status().textContent = message;
  }

  function copy(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    return Promise.reject(new Error("no clipboard"));
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest ? event.target.closest(SELECTOR) : null;
    if (!button) {
      return;
    }

    var text = value(button);
    if (!text) {
      return;
    }

    copy(text).then(
      function () {
        announce("Copied to the clipboard");
      },
      function () {
        // Without a clipboard API, select the text so that it can still be
        // copied by hand.
        if (select(button)) {
          announce("Selected — press Ctrl+C to copy");
        } else {
          announce("Copying is not available in this browser");
        }
      }
    );
  });

  // The live region has to exist before anything is announced into it.
  function start() {
    status();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
