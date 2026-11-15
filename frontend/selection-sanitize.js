// Imported Word, PDF and photo content is stored as HTML. Render it only after
// removing scripts, event handlers, styles and unknown elements, so that a
// crafted document cannot run code in the teacher's signed-in session.
const safeTags = new Set(['P','DIV','SPAN','BR','B','I','U','EM','STRONG','SUP','SUB','SMALL','H1','H2','H3','H4','H5','H6','TABLE','THEAD','TBODY','TFOOT','TR','TD','TH','UL','OL','LI','IMG','FIGURE','FIGCAPTION','HR','A','CODE','BLOCKQUOTE']);
const safeClasses = new Set(['opt-grid','four','figure-note','choice-line','sub-q','blank-line','math']);
function safeSrc(src) {
  return /^data:image\/(png|jpe?g|webp|gif);base64,[a-z0-9+/=]+$/i.test(src) || /^library\/(picture|assets)\//.test(src);
}
function safeHref(href) {
  return /^(https?:|mailto:)/i.test(href.trim());
}
function safeSpan(value) {
  const n = Number(String(value).trim());
  return Number.isInteger(n) && n >= 1 && n <= 50;
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
    const tag = child.tagName;
    let validHref = false;
    for (const attr of Array.from(child.attributes)) {
      const name = attr.name.toLowerCase();
      if (name === 'src') {
        if (tag !== 'IMG' || !safeSrc(attr.value)) child.removeAttribute(attr.name);
      } else if (name === 'href') {
        if (tag === 'A' && safeHref(attr.value)) validHref = true;
        else child.removeAttribute(attr.name);
      } else if (name === 'rel' || name === 'target') {
        child.removeAttribute(attr.name);
      } else if (name === 'colspan' || name === 'rowspan') {
        if ((tag !== 'TD' && tag !== 'TH') || !safeSpan(attr.value)) child.removeAttribute(attr.name);
      } else if (name === 'loading') {
        if (tag !== 'IMG' || !['lazy','eager'].includes(attr.value.toLowerCase())) child.removeAttribute(attr.name);
      } else if (name === 'decoding') {
        if (tag !== 'IMG' || !['async','sync','auto'].includes(attr.value.toLowerCase())) child.removeAttribute(attr.name);
      } else if (name === 'alt') {
        if (tag !== 'IMG') child.removeAttribute(attr.name);
      } else if (name === 'class') {
        const kept = attr.value.split(/\s+/).filter(c => safeClasses.has(c));
        if (kept.length) child.setAttribute('class', kept.join(' '));
        else child.removeAttribute(attr.name);
      } else if (name !== 'title') {
        child.removeAttribute(attr.name);
      }
    }
    if (validHref) {
      child.setAttribute('rel', 'noopener noreferrer');
      child.setAttribute('target', '_blank');
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
