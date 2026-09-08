// Read-only observations of the rendered page. No probes, theme changes,
// emulated preferences, or synthetic focus are introduced by this module.
// Floors: docs/accessibility.md sections 8-9 and design/tokens.css.

const TOKENS = [
  'surface-0', 'surface-1', 'surface-2', 'surface-3', 'surface-inset',
  'text-primary', 'text-secondary', 'text-disabled', 'border-subtle',
  'border-default', 'border-strong', 'focus-ring', 'focus-halo',
  'accent-text', 'accent-fill', 'accent-on-fill', 'warning-text', 'warning-bg',
  'danger-text', 'danger-bg', 'success-text', 'success-bg', 'brand-border',
];

function rgba(value) {
  if (value === 'transparent') return [0, 0, 0, 0];
  const match = /^(rgba?)\(([^)]+)\)$/.exec(value.trim());
  if (!match) return null;
  const parts = match[2].replace(/\//g, ' ').split(/[,\s]+/).filter(Boolean);
  if (parts.length < 3 || parts.length > 4) return null;
  const result = parts.map((part, i) => Number.parseFloat(part) * (part.endsWith('%') ? (i < 3 ? 2.55 : 0.01) : 1));
  if (result.some(number => !Number.isFinite(number))) return null;
  if (result.length === 3) result.push(1);
  return result;
}

function composite(front, back) {
  const alpha = front[3] + back[3] * (1 - front[3]);
  if (!alpha) return [0, 0, 0, 0];
  return [0, 1, 2].map(i => (front[i] * front[3] + back[i] * back[3] * (1 - front[3])) / alpha).concat(alpha);
}

function luminance(color) {
  const channels = color.slice(0, 3).map(value => {
    const channel = value / 255;
    return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
  });
  return channels[0] * 0.2126 + channels[1] * 0.7152 + channels[2] * 0.0722;
}

export function contrastRatio(foreground, background) {
  const front = Array.isArray(foreground) ? foreground : rgba(foreground);
  const back = Array.isArray(background) ? background : rgba(background);
  if (!front || !back || back[3] < 0.999) return null;
  const a = luminance(composite(front, back)), b = luminance(back);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

function visible(element, win) {
  if (element.closest('[hidden], [aria-hidden="true"], dialog:not([open])')) return false;
  for (let parent = element.parentElement; parent; parent = parent.parentElement) {
    if (parent.tagName === 'DETAILS' && !parent.open && !parent.querySelector('summary')?.contains(element)) return false;
  }
  const style = win.getComputedStyle(element);
  if (style.visibility !== 'visible' || style.display === 'none' || Number(style.opacity) === 0) return false;
  return Array.from(element.getClientRects()).some(rect => {
    let left = Math.max(0, rect.left), top = Math.max(0, rect.top);
    let right = Math.min(win.innerWidth, rect.right), bottom = Math.min(win.innerHeight, rect.bottom);
    for (let parent = element.parentElement; parent; parent = parent.parentElement) {
      const parentStyle = win.getComputedStyle(parent), clip = parent.getBoundingClientRect();
      if (/(hidden|clip|scroll|auto)/.test(parentStyle.overflowX)) { left = Math.max(left, clip.left); right = Math.min(right, clip.right); }
      if (/(hidden|clip|scroll|auto)/.test(parentStyle.overflowY)) { top = Math.max(top, clip.top); bottom = Math.min(bottom, clip.bottom); }
    }
    return right > left && bottom > top;
  });
}

function backgroundFor(element, win) {
  const chain = [];
  for (let current = element; current; current = current.parentElement) chain.unshift(current);
  let background = [0, 0, 0, 0];
  for (const current of chain) {
    const style = win.getComputedStyle(current);
    if (style.backgroundImage !== 'none' || Number(style.opacity) !== 1 || (style.filter && style.filter !== 'none') || (style.mixBlendMode && style.mixBlendMode !== 'normal')) return null;
    const color = rgba(style.backgroundColor);
    if (!color) return null;
    background = composite(color, background);
  }
  return background[3] >= 0.999 ? background : null;
}

function seconds(value) {
  return value.split(',').map(part => {
    const text = part.trim();
    return Number.parseFloat(text) / (text.endsWith('ms') ? 1000 : 1);
  });
}

export function observeAppearance(root = document) {
  const doc = root.ownerDocument || root;
  const win = doc.defaultView;
  const computed = win.getComputedStyle(doc.documentElement);
  const media = {};
  for (const query of ['prefers-color-scheme: light', 'prefers-color-scheme: dark',
    'prefers-reduced-motion: reduce', 'prefers-reduced-motion: no-preference',
    'prefers-contrast: more', 'prefers-contrast: less', 'prefers-contrast: custom',
    'prefers-contrast: no-preference', 'forced-colors: active', 'forced-colors: none']) {
    const match = win.matchMedia('(' + query + ')');
    media[query] = { matches: match.matches, serialized: match.media };
  }
  const tokenDeclarations = Object.fromEntries(TOKENS.map(name => ['--color-' + name, computed.getPropertyValue('--color-' + name).trim()]));
  for (const name of ['--focus-ring-width', '--focus-ring-offset', '--duration-fast', '--duration-normal']) tokenDeclarations[name] = computed.getPropertyValue(name).trim();
  const dialogs = Array.from(doc.querySelectorAll('dialog[open]')).filter(el => visible(el, win));
  const activeDialog = dialogs[dialogs.length - 1];
  const all = Array.from(root.querySelectorAll('*')).filter(el => visible(el, win) && (!activeDialog || activeDialog.contains(el)));
  const observations = [], seen = new Set();
  const motion = [];
  let eligible = 0;
  for (const element of all) {
    const style = win.getComputedStyle(element);
    const ownText = Array.from(element.childNodes).some(node => node.nodeType === 3 && node.textContent.trim());
    const control = element.matches('button,input,select,textarea,a[href],[role="button"],[role="checkbox"],[role="radio"],[role="option"]');
    // Native indicator controls have a computed color but paint no text glyph.
    // Empty fields' placeholder colors also need their own pseudo-element read.
    const fieldText = element.matches('input,textarea') &&
      !element.matches('input[type="checkbox"],input[type="radio"],input[type="range"],input[type="color"],input[type="image"],input[type="hidden"]') &&
      !!String(element.value || '').trim();
    const selectedText = element.tagName === 'SELECT' && Array.from(element.selectedOptions || []).some(option => option.textContent.trim());
    const rendersText = ownText || fieldText || selectedText;
    const animated = style.animationName !== 'none' && seconds(style.animationDuration).some(value => value > 0.001);
    const transitioning = seconds(style.transitionDuration).some(value => value > 0.001);
    if ((animated || transitioning) && motion.length < 60) motion.push({
      tag: element.tagName.toLowerCase(), class: element.getAttribute('class') || '',
      animation: style.animationName, duration: style.animationDuration,
      iterations: style.animationIterationCount, transition: style.transitionDuration,
      exceedsReducedMotionFloor: !!media['prefers-reduced-motion: reduce'].matches,
    });
    if (!ownText && !control) continue;
    eligible++;
    const background = backgroundFor(element, win);
    const parentBackground = backgroundFor(element.parentElement || element, win);
    const color = rgba(style.color);
    const ratio = rendersText && background && color ? contrastRatio(color, background) : null;
    const inactive = !!element.closest('[disabled],[aria-disabled="true"]');
    const key = [element.tagName, element.getAttribute('class'), rendersText, style.color, JSON.stringify(background), inactive, style.borderTopColor, style.outlineColor].join('|');
    if (seen.has(key) || observations.length >= 160) continue;
    seen.add(key);
    const label = (element.getAttribute('aria-label') || element.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 100);
    const borderRatio = parentBackground ? contrastRatio(style.borderTopColor, parentBackground) : null;
    const outlineRatio = parentBackground ? contrastRatio(style.outlineColor, parentBackground) : null;
    const outlined = style.outlineStyle !== 'none' && Number.parseFloat(style.outlineWidth) > 0;
    observations.push({
      tag: element.tagName.toLowerCase(), class: element.getAttribute('class') || '', label,
      inactive, color: style.color, background: style.backgroundColor, effectiveBackground: background,
      fontSize: style.fontSize, fontWeight: style.fontWeight, appearance: style.appearance || style.webkitAppearance,
      textAssessment: rendersText ? 'Visible text value or text node.' : 'No text glyph assessed; control boundary remains an observation.',
      textContrast: ratio === null ? null : Number(ratio.toFixed(3)), textFloor: inactive || !rendersText ? null : 4.5,
      textPass: inactive || ratio === null ? null : ratio >= 4.5,
      border: { color: style.borderTopColor, width: style.borderTopWidth, style: style.borderTopStyle, againstParent: borderRatio === null ? null : Number(borderRatio.toFixed(3)), floor: 3, assessment: 'Observation only: distinguish required control boundaries from decorative separators.' },
      outline: { color: style.outlineColor, width: style.outlineWidth, style: style.outlineStyle, againstParent: outlineRatio === null ? null : Number(outlineRatio.toFixed(3)), floor: 3, pass: outlined && !inactive && outlineRatio !== null ? outlineRatio >= 3 : null },
    });
  }
  return {
    schema: 1, method: 'Read-only computed styles from visible rendered elements; WCAG relative luminance, normal text floor4.5 and UI floor3.',
    viewport: { width: win.innerWidth, height: win.innerHeight, devicePixelRatio: win.devicePixelRatio },
    theme: doc.documentElement.getAttribute('data-theme') || 'system', colorScheme: computed.colorScheme,
    media, tokenDeclarations,
    cssSupport: Object.fromEntries(['color:light-dark(white, black)', 'color-scheme:light dark', 'selector(:has(*))', 'selector(:focus-visible)', 'forced-color-adjust:auto'].map(value => [value, win.CSS?.supports(value.startsWith('selector(') ? value : '(' + value + ')') || false])),
    eligibleElements: eligible, representativeSamples: observations.length, observations, motion,
    textFailures: observations.filter(row => row.textPass === false),
    reducedMotionFailures: motion.filter(row => row.exceedsReducedMotionFloor),
    limits: ['Token declarations may contain unresolved light-dark(); contrast uses actual computed element colors instead.',
      'No pixels or native widget internals are sampled. Gradients, filters, opacity groups, blend modes, and unknown canvas backgrounds are unassessed.',
      'Samples cover the current visible state; closed disclosures, unfocused rings, offscreen content, hover states, and modal backdrops need separate captures.',
      'Border ratios are observations, not blanket pass/fail; decorative lines and inactive controls have different requirements.',
      'Indicator-only inputs, empty-field placeholders, and generated content are not assessed as text.',
      'Reduced-motion observations cover currently rendered CSS animation/transition durations, not all possible application motion.'],
  };
}

export default observeAppearance;
