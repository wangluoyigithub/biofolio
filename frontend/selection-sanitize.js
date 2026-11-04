// Imported Word, PDF and photo content is stored as HTML. Render it only after
// removing scripts, event handlers, styles and unknown elements, so that a
// crafted document cannot run code in the teacher's signed-in session.
const safeTags = new Set(['P','DIV','SPAN','BR','B','I','U','EM','STRONG','SUP','SUB','SMALL','H1','H2','H3','H4','H5','H6','TABLE','THEAD','TBODY','TFOOT','TR','TD','TH','UL','OL','LI','IMG','FIGURE','FIGCAPTION','HR','A','CODE','BLOCKQUOTE']);
const safeAttrs = new Set(['alt','colspan','rowspan','class','title','loading','decoding']);
function safeSrc(src) {
  return /^data:image\/(png|jpe?g|webp|gif);base64,[a-z0-9+/=]+$/i.test(src) || /^library\/(picture|assets)\//.test(src);
}
function safeHref(href) {
  return /^(https?:|mailto:)/i.test(href.trim());
}
function cleanNode(node) {
  for (const child of Array.from(node.childNodes)) {
    if (child.nodeType === Node.TEXT_NODE) continue;
    if (child.nodeType !== Node.ELEMENT_NODE || !safeTags.has(child.tagName)) {
      if (child.nodeType === Node.ELEMENT_NODE) {
        // Keep the text of unknown wrappers, drop scripts, styles and embedded objects.
        if (/^(SCRIPT|STYLE|IFRAME|OBJECT|EMBED|LINK|META|SVG|MATH|FORM|INPUT|BUTTON|TEXTAREA|SELECT|NOSCRIPT|TEMPLATE)$/.test(child.tagName)) { child.remove(); continue; }
      }
      if (child.nodeType === Node.ELEMENT_NODE) { cleanNode(child); child.replaceWith(...child.childNodes); continue; }
      child.remove();
      continue;
    }
    for (const attr of Array.from(child.attributes)) {
      const name = attr.name.toLowerCase();
      if (child.tagName === 'IMG' && name === 'src') {
        if (!safeSrc(attr.value)) child.removeAttribute(attr.name);
      } else if (child.tagName === 'A' && name === 'href') {
        if (!safeHref(attr.value)) child.removeAttribute(attr.name);
        else { child.setAttribute('rel', 'noopener noreferrer'); child.setAttribute('target', '_blank'); }
      } else if (!safeAttrs.has(name) && name !== 'src' && name !== 'href' && name !== 'rel' && name !== 'target') {
        child.removeAttribute(attr.name);
      }
    }
    cleanNode(child);
  }
}
// Returns a string safe for v-html or innerHTML. Parsing happens in an inert template.
function safeHtml(html) {
  if (!html) return '';
  const el = document.createElement('template');
  el.innerHTML = String(html);
  cleanNode(el.content);
  return el.innerHTML;
}
