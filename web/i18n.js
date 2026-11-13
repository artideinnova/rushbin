// The words of the pages in the reader's language. English is the source (i18n/en.js); each language is one
// file of the same keys (i18n_test.go checks keys, {placeholders} and markup). The language follows the
// browser's list (in the apps, the system's), or the reader's choice in a language menu, kept in this browser.
// Plurals by Intl.PluralRules, numbers and dates by Intl, in that language.
const I18N = (() => {
  const NAMES = { en: 'English', it: 'Italiano', es: 'Español', fr: 'Français', de: 'Deutsch', 'pt-BR': 'Português (Brasil)', ja: '日本語' };
  const RTL = ['ar', 'fa', 'he', 'ur']; // written right to left: none yet, Arabic will be
  const fit = l => NAMES[l] ? l : NAMES[l.split('-')[0]] ? l.split('-')[0] : /^pt\b/i.test(l) ? 'pt-BR' : '';
  let chosen = null; try { chosen = localStorage.getItem('rushbin.lang'); } catch {}
  if (!NAMES[chosen]) chosen = null;
  // The landing has an address per language (/it, /pt-br …, server.go): it speaks it, unless the reader chose one.
  const page = (location.pathname.match(/^\/(it|es|fr|de|pt-br|ja)$/) || [])[1];
  const lang = chosen || (page && fit(page === 'pt-br' ? 'pt-BR' : page)) || [...(navigator.languages || []), navigator.language || ''].map(fit).find(Boolean) || 'en';
  document.documentElement.lang = lang;
  document.documentElement.dir = RTL.includes(lang.split('-')[0]) ? 'rtl' : 'ltr';
  // A parser-blocking load, like this script's: the page's own script and its first paint already have the words.
  document.write(`<script src="/i18n/${lang}.js"><\/script>`);

  let W = {}, FROM = null;
  const plural = new Intl.PluralRules(lang);
  const fill = (s, v) => s.replace(/\{(\w+)\}/g, (m, k) => k in v ? (typeof v[k] === 'number' ? v[k].toLocaleString(lang) : v[k]) : m);
  // t(key, {n: 3, name: 'x'}): numbers are written the language's way; n also picks the plural form.
  // Values go in as they are: escape them where the result becomes HTML.
  function t(key, v = {}) {
    let s = W[key] ?? key;
    if (typeof s === 'object') s = s[plural.select(v.n)] ?? s.other;
    return fill(s, v);
  }
  // A message of the engine or the site, written in English in Go: the language's words when the dictionary has
  // them ("server", keyed by the English with {placeholders}), the English otherwise. What fills a placeholder
  // is itself a message, or a number.
  function from(text) {
    const S = W.server || {};
    if (typeof text !== 'string' || !text) return text;
    if (S[text]) return S[text];
    FROM ||= Object.keys(S).filter(k => k.includes('{')).map(k => {
      const parts = k.split(/\{(\w+)\}/);
      const re = parts.map((p, i) => i % 2 ? '(.+?)' : p.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('');
      return [new RegExp(`^${re}$`, 's'), parts.filter((p, i) => i % 2), k];
    });
    for (const [re, names, k] of FROM) {
      const m = text.match(re);
      if (m) return fill(S[k], Object.fromEntries(names.map((x, i) => [x, /^\d+$/.test(m[i + 1]) ? +m[i + 1] : from(m[i + 1])])));
    }
    return text;
  }
  // Static words in the markup: data-i18n names the key of the content, data-i18n-<attribute> the key of an attribute.
  function apply(root = document) {
    for (const e of root.querySelectorAll('*')) for (const a of [...e.attributes]) {
      if (a.name === 'data-i18n') e.innerHTML = t(a.value);
      else if (a.name.startsWith('data-i18n-')) e.setAttribute(a.name.slice(10), t(a.value));
    }
  }
  document.addEventListener('DOMContentLoaded', () => apply());
  // The language menu: Automatic follows the browser again.
  const picker = () => `<select data-lang aria-label="${t('lang.label')}"><option value="">${t('lang.auto')}</option>${
    Object.entries(NAMES).map(([k, v]) => `<option value="${k}" lang="${k}"${k === chosen ? ' selected' : ''}>${v}</option>`).join('')}</select>`;
  document.addEventListener('change', e => {
    if (!e.target.matches?.('[data-lang]')) return;
    try { e.target.value ? localStorage.setItem('rushbin.lang', e.target.value) : localStorage.removeItem('rushbin.lang'); } catch {}
    location.reload();
  });
  return {
    lang, t, from, apply, picker,
    words: w => { W = w; },
    legal: lang === 'it' ? '.it' : '', // the legal pages exist in English and Italian
    num: v => Number(v ?? 0).toLocaleString(lang),
    date: (v, o) => new Date(v).toLocaleString(lang, o),
  };
})();
