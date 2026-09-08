/** Frozen picker layout: 260px selection pane from 1024 effective CSS pixels.
 * Full-page zoom already changes innerWidth; text-only enlargement reduces
 * available reading room by the measured body font scale (baseline 14px).
 */
export const SELECTION_PANE_WIDTH = 260;
export const SELECTION_PANE_BREAKPOINT = 1024;

export function selectionPaneFits(win = window) {
  const font = parseFloat(win.getComputedStyle(win.document.body).fontSize) || 14;
  return win.innerWidth >= SELECTION_PANE_BREAKPOINT * Math.max(1, font / 14);
}

export function observeSelectionLayout(onChange, win = window) {
  let previous;
  const update = () => {
    const wide = selectionPaneFits(win);
    if (wide !== previous) { previous = wide; onChange(wide); }
  };
  win.addEventListener('resize', update);
  const observer = typeof win.ResizeObserver === 'function' ? new win.ResizeObserver(update) : null;
  observer?.observe(win.document.body);
  const mutations = typeof win.MutationObserver === 'function' ? new win.MutationObserver(update) : null;
  mutations?.observe(win.document.documentElement, { attributes: true, attributeFilter: ['style', 'class'] });
  mutations?.observe(win.document.body, { attributes: true, attributeFilter: ['style', 'class'] });
  update();
  return () => { win.removeEventListener('resize', update); observer?.disconnect(); mutations?.disconnect(); };
}
