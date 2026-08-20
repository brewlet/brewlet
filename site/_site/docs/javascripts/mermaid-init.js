(function () {
  function renderMermaid() {
    if (!window.mermaid) return;

    window.mermaid.initialize({ startOnLoad: false });
    window.mermaid.run({ querySelector: ".mermaid" });
  }

  if (typeof document$ !== "undefined" && document$.subscribe) {
    document$.subscribe(renderMermaid);
    return;
  }

  window.addEventListener("load", renderMermaid);
})();
