// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// renderPapio sets an element's text with every occurrence of the product name
// "papio" italicised (wrapped in <em>), per the papio naming convention. It
// builds text nodes and <em> elements rather than assigning innerHTML, so there
// is no HTML-injection surface, and Element.textContent still returns the flat
// string — callers and tests comparing textContent are unaffected.
export function renderPapio(el: Element, text: string): void {
  const doc = el.ownerDocument;
  el.replaceChildren();
  const parts = text.split("papio");
  parts.forEach((part, index) => {
    if (index > 0) {
      const em = doc.createElement("em");
      em.textContent = "papio";
      el.append(em);
    }
    if (part) el.append(doc.createTextNode(part));
  });
}
