// AI Expedite browser kit: reads, fills and (only after the person approved it) submits a
// form in the person's own Chrome. Ported from the form-fill proof of concept (poc-0.2.4);
// every rule of the POC is kept.
//
// Runs in an ISOLATED JavaScript world: Chrome's AppleScript world, or a DevTools isolated
// world. The page's own scripts cannot see or patch anything defined here; the kit shares only
// the DOM with them.
//
// Every entry point is synchronous and returns a JSON string, because AppleScript's
// "execute javascript" never awaits a promise. The agent drives multi-step work (open a
// dropdown, wait, pick an option, verify) by calling again with the next step.
//
// The kit never submits on its own. It never presses Enter, never clicks anything whose
// activation reaches a submit-class control or a link while filling, refuses hidden fields and
// sensitive kinds, and while a fill or a click runs (plus a short grace period) it cancels
// page-initiated submissions and navigations. The ONE way a form is submitted is
// armSubmit(ref, token): the agent calls it only for a submit request the person approved, after
// it re-checked the form's digest, and it lifts the guard for exactly that control, once.
//
// Nothing the kit returns may be logged by the agent: it carries page text and form values.
(function () {
  'use strict';
  var VERSION = 'aix-1.0.3';
  if (globalThis.__aixKit && globalThis.__aixKit.version === VERSION) return;
  if (globalThis.__aixKit && globalThis.__aixKit._teardown) {
    try { globalThis.__aixKit._teardown(); } catch (e) { /* older kit */ }
  }

  var DOC_ID = Math.random().toString(36).slice(2, 10) + '-' + Date.now().toString(36);
  // Refs carry this kit instance's tag, so a ref from an earlier document (or an earlier kit in
  // a recreated world) never resolves to some other element of this one.
  var TAG = Math.random().toString(36).slice(2, 6);
  var HOST_ID = '__aix_formfill_host';
  var FIELD_SELECTOR = [
    'input', 'select', 'textarea', '[contenteditable=""]', '[contenteditable="true"]', '[contenteditable="plaintext-only"]',
    '[role="combobox"]', '[role="textbox"]', '[role="searchbox"]', '[role="checkbox"]',
    '[role="switch"]', '[role="radiogroup"]', '[role="spinbutton"]'
  ].join(',');
  var EDITABLE_SELECTOR = '[contenteditable="true"],[contenteditable=""],[contenteditable="plaintext-only"]';
  var SKIP_INPUT_TYPES = { hidden: 1, submit: 1, button: 1, image: 1, reset: 1 };
  var SUBMIT_WORDS = /\b(next|continue|submit|send|save|register|create|sign ?up|sign ?in|log ?in|confirm|pay|place order|checkout|finish|done|apply|request|enroll|join|agree|accept|publish|post|buy|purchase|subscribe|verify)\b/i;
  var COMBO_WAIT_MS = 10000;    // the doc's "up to 10 seconds" for dynamic content
  var COVER_RETRY_MS = 1500;    // a closing menu may cover a field for a moment
  var GRACE_MS = 2500;          // guards stay armed this long after a fill ends
  var AGENT_GONE_MS = 30000;    // no call from the agent this long while filling: it is gone

  var state = {
    refs: new Map(), elRef: new WeakMap(), next: 1,
    groups: new Map(), groupKeyRef: new Map(), nextGroup: 1,
    active: false, guardUntil: 0, guardsInstalled: false, teardownTimer: 0,
    stopRequested: false, stopAt: 0, stopTrusted: null,
    lastGestureAt: 0, lastCallAt: 0, watchdog: 0, mode: null, agentActingUntil: 0,
    allowNavHref: null, approved: null, sawInvalid: false,
    blockedSubmits: [], allowedByPerson: [], allowedByAgent: [], requests: [], perf: null,
    retargeted: [],
    host: null, outline: null, msg: null, outlineTarget: null, raf: 0,
    pending: new Map()
  };

  // ---------------------------------------------------------------- small helpers
  function norm(s) { return String(s == null ? '' : s).replace(/[\s ​‌‍﻿]+/g, ' ').trim(); }
  function lower(s) { return norm(s).toLowerCase(); }
  function clip(s, n) { s = norm(s); return s.length > n ? s.slice(0, n - 1) + '…' : s; }
  function fnv(str) {
    var h = 0x811c9dc5;
    for (var i = 0; i < str.length; i++) { h ^= str.charCodeAt(i); h = Math.imul(h, 0x01000193); }
    return (h >>> 0).toString(16);
  }
  function digits(s) { return String(s == null ? '' : s).replace(/\D/g, ''); }
  function toNumber(s) {
    var t = String(s == null ? '' : s).replace(/[$€£¥\s]/g, '');
    if (!/^-?(\d{1,3}(,\d{3})+|\d+)(\.\d+)?$/.test(t)) return null;
    return parseFloat(t.replace(/,/g, ''));
  }
  // Is what the field shows the value we meant? Masks may regroup punctuation, never digits.
  function looksSame(actual, expected) {
    var a = norm(actual), e = norm(expected);
    if (a === e || a.toLowerCase() === e.toLowerCase()) return true;
    if (!a || !e) return false;
    var runsA = (a.match(/\d+/g) || []).join(','), runsE = (e.match(/\d+/g) || []).join(',');
    var restA = a.replace(/[\d\s()+\-.\/,]/g, ''), restE = e.replace(/[\d\s()+\-.\/,]/g, '');
    if (runsE && runsA === runsE && restA === restE) return true;         // same digit groups, other punctuation
    if (/^\+?\d{4,}$/.test(e.replace(/\s/g, '')) && /^[\d\s()+\-]+$/.test(a) && digits(a) === digits(e)) return true; // one run into a phone-style mask
    var na = toNumber(a), ne = toNumber(e);
    return na !== null && ne !== null && na === ne;                        // 1,200 vs 1200
  }
  function inOwnUi(el) { return !!(el && el.closest && el.closest('#' + HOST_ID)); }
  function root(el) { var r = el.getRootNode && el.getRootNode(); return r && r.getElementById ? r : document; }
  function parentOf(n) { return n.parentElement || (n.getRootNode && n.getRootNode() && n.getRootNode().host) || null; }

  function refFor(el) {
    var r = state.elRef.get(el);
    if (!r) { r = 'f' + (state.next++) + '_' + TAG; state.elRef.set(el, r); state.refs.set(r, new WeakRef(el)); }
    return r;
  }
  function byRef(ref) {
    var w = state.refs.get(ref); var el = w && w.deref();
    return el && el.isConnected ? el : null;
  }
  function* allElements(r) {
    var walker = document.createTreeWalker(r, NodeFilter.SHOW_ELEMENT);
    var node = walker.nextNode();
    while (node) {
      yield node;
      if (node.shadowRoot && node.id !== HOST_ID) yield* allElements(node.shadowRoot);
      node = walker.nextNode();
    }
  }

  // ---------------------------------------------------------------- labels (subset of W3C accname)
  function textWithoutControls(node) {
    var out = '';
    var kids = node.childNodes;
    if (node.tagName === 'SLOT') {
      var assigned = node.assignedNodes({ flatten: true });
      if (assigned.length) kids = assigned;
    }
    for (var i = 0; i < kids.length; i++) {
      var c = kids[i];
      if (c.nodeType === 3) { out += c.nodeValue + ' '; continue; }
      if (c.nodeType !== 1) continue;
      var tag = c.tagName;
      if (tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA' || tag === 'BUTTON' || tag === 'SCRIPT' || tag === 'STYLE' || tag === 'OPTION' || tag === 'svg' || tag === 'SVG') continue;
      var role = c.getAttribute('role');
      if (role === 'listbox' || role === 'combobox' || role === 'option') continue;
      if (c.getAttribute('aria-hidden') === 'true') continue;
      out += textWithoutControls(c) + ' ';
    }
    return out;
  }
  function cleanLabel(s) { return norm(s).replace(/\s*[*✱]+\s*$/, '').replace(/\s*\((required|optional)\)\s*$/i, ''); }
  function idrefText(el, attr, excludeSelf) {
    var ids = norm(el.getAttribute(attr)).split(' ').filter(Boolean);
    var parts = [];
    for (var i = 0; i < ids.length; i++) {
      if (excludeSelf && ids[i] === el.id) continue;
      var t = root(el).getElementById(ids[i]) || document.getElementById(ids[i]);
      if (t) parts.push(norm(textWithoutControls(t)) || norm(t.textContent));
    }
    return norm(parts.join(' '));
  }
  function nearbyText(el) {
    var node = el;
    for (var depth = 0; depth < 3 && node; depth++) {
      var sib = node.previousElementSibling;
      for (var i = 0; i < 3 && sib; i++, sib = sib.previousElementSibling) {
        if (sib.matches(FIELD_SELECTOR) || sib.querySelector(FIELD_SELECTOR)) return '';
        var t = norm(sib.innerText);
        if (t) return t.length <= 80 ? t : '';
      }
      node = node.parentElement;
      if (!node || node.matches('form, fieldset, body, html')) break;
    }
    return '';
  }
  // Returns [name, source]: which kind of label gave the name matters, because an aria-*
  // name is not something the person sees.
  function accNameInfo(el) {
    var n = idrefText(el, 'aria-labelledby', true); if (n) return [cleanLabel(n), 'aria-labelledby'];
    n = norm(el.getAttribute('aria-label')); if (n) return [cleanLabel(n), 'aria-label'];
    if (el.labels && el.labels.length) {
      n = norm(Array.prototype.map.call(el.labels, textWithoutControls).join(' '));
      if (n) return [cleanLabel(n), 'label'];
    }
    if (el.tagName === 'FIELDSET') {
      var lg = el.querySelector(':scope > legend');
      if (lg) { n = norm(lg.textContent); if (n) return [cleanLabel(n), 'legend']; }
    }
    n = norm(el.getAttribute('title')); if (n) return [cleanLabel(n), 'title'];
    n = norm(el.getAttribute('placeholder')); if (n) return [cleanLabel(n), 'placeholder'];
    n = nearbyText(el); if (n) return [cleanLabel(n), 'nearby'];
    return ['', 'none'];
  }
  function accName(el) { return accNameInfo(el)[0]; }
  // The label text a person can actually see next to the field.
  function visibleLabelOf(el) {
    var parts = [];
    if (el.labels) for (var i = 0; i < el.labels.length; i++) {
      var l = el.labels[i];
      if (isShown(l)) parts.push(norm(l.innerText));
    }
    var n = cleanLabel(parts.join(' '));
    if (n) return n;
    var ids = norm(el.getAttribute('aria-labelledby')).split(' ').filter(Boolean);
    for (var j = 0; j < ids.length; j++) {
      if (ids[j] === el.id) continue;
      var t = root(el).getElementById(ids[j]) || document.getElementById(ids[j]);
      if (t && isShown(t)) parts.push(norm(t.innerText));
    }
    n = cleanLabel(parts.join(' '));
    return n || cleanLabel(nearbyText(el));
  }
  function describedBy(el) {
    var parts = [idrefText(el, 'aria-describedby')];
    if (el.getAttribute('aria-invalid') === 'true') parts.push(idrefText(el, 'aria-errormessage'));
    return clip(parts.join(' '), 200) || null;
  }

  // ---------------------------------------------------------------- field kinds
  function isSelectOnlyInput(el) {
    // Element Plus / Vuetify / Quasar render a select as a readonly input with role=combobox.
    return el.tagName === 'INPUT' && el.readOnly && el.getAttribute('aria-readonly') !== 'true' &&
      lower(el.getAttribute('role')) === 'combobox' &&
      (el.hasAttribute('aria-controls') || el.hasAttribute('aria-expanded') || el.hasAttribute('aria-haspopup'));
  }
  function kindOf(el) {
    var tag = el.tagName, role = lower(el.getAttribute('role'));
    if (tag === 'INPUT') {
      var t = lower(el.type) || 'text';
      if (SKIP_INPUT_TYPES[t]) return null;
      if (role === 'combobox') return 'combobox';
      if (t === 'checkbox') return role === 'switch' ? 'switch' : 'checkbox';
      if (t === 'radio') return 'radio';
      if (t === 'file' || t === 'color' || t === 'range') return 'unsupported';
      if (t === 'date' || t === 'time' || t === 'datetime-local' || t === 'month' || t === 'week') return 'datetime';
      return 'text';
    }
    if (tag === 'SELECT') return el.multiple ? 'unsupported' : 'select';
    if (tag === 'TEXTAREA') return 'text';
    if (role === 'combobox') return 'combobox';
    if (role === 'checkbox') return 'checkbox';
    if (role === 'switch') return 'switch';
    if (role === 'radiogroup') return 'radio-group';
    if (role === 'textbox' || role === 'searchbox' || el.isContentEditable) return 'contenteditable';
    if (role === 'spinbutton') return 'unsupported';
    return null;
  }
  function isChecked(el) {
    if (el.tagName === 'INPUT') return !!el.checked;
    return lower(el.getAttribute('aria-checked')) === 'true';
  }
  function comboDisplay(el) { return norm(textWithoutControls(el)) || norm(el.textContent); }
  function comboContainer(el) {
    var best = el, node = el.parentElement;
    for (var i = 0; i < 5 && node && !node.matches('form, body, html, fieldset'); i++, node = node.parentElement) {
      if (node.querySelectorAll(FIELD_SELECTOR).length !== 1) break;
      best = node;
    }
    return best;
  }
  // What a combobox's wrapper shows, minus the field's own label text.
  function containerValue(el, label) {
    var t = norm(textWithoutControls(comboContainer(el)));
    var l = norm(label);
    if (l && t.toLowerCase().indexOf(l.toLowerCase()) === 0) t = norm(t.slice(l.length));
    return t;
  }
  function currentValue(el, kind) {
    if (kind === 'text' || kind === 'datetime') return el.value;
    if (kind === 'select') { var o = el.selectedOptions && el.selectedOptions[0]; return o ? norm(o.label || o.text) : ''; }
    if (kind === 'combobox') return el.tagName === 'INPUT' ? (el.value || containerValue(el, accName(el))) : comboDisplay(el);
    if (kind === 'contenteditable') return norm(el.innerText);
    return '';
  }

  // ---------------------------------------------------------------- sensitive fields (deterministic, never the model)
  function tokenize(s) {
    return String(s == null ? '' : s).replace(/([a-z0-9])([A-Z])/g, '$1 $2').replace(/[_\-.\[\]:\/#|(),;'"?!]+/g, ' ');
  }
  function sensitiveHay(el, label) {
    var parts = [el.getAttribute('name'), el.id, label, el.getAttribute('aria-label'), el.getAttribute('title'),
      el.getAttribute('placeholder'), idrefText(el, 'aria-labelledby'), idrefText(el, 'aria-describedby'), nearbyText(el)];
    if (el.labels) for (var i = 0; i < el.labels.length; i++) parts.push(textWithoutControls(el.labels[i]));
    var raw = parts.filter(Boolean).join(' | ');
    return { raw: raw, hay: ' ' + lower(tokenize(raw)) + ' ' };
  }
  var BENIGN_HEADS = { email: 1, mail: 1, address: 1, country: 1, name: 1, nickname: 1, title: 1, label: 1, type: 1,
    preference: 1, preferences: 1, reminder: 1, wishlist: 1, list: 1, experience: 1, years: 1, frequency: 1, language: 1,
    format: 1, photo: 1, image: 1, team: 1, description: 1, color: 1, colour: 1, notes: 1, comment: 1, comments: 1,
    city: 1, nationality: 1, issuer: 1, holder: 1, provider: 1, brand: 1 };
  function headNoun(label) {
    var s = cleanLabel(label).replace(/\s*\([^)]*\)\s*$/, '');
    var words = lower(tokenize(s)).split(' ').filter(Boolean);
    return words.length ? words[words.length - 1] : '';
  }
  // Strong rules always apply. Weak rules (a keyword that also appears in ordinary labels) are
  // cancelled when the label is plainly about something else ("Credit card billing address").
  var RULES = [
    { re: /\b(password|passphrase|passwort|mot de passe|contrase[nñ]a|has[lł]o|senha)\b/, level: 'never', kind: 'password' },
    { re: /\b(api ?key|secret ?key|access ?token|auth ?token|bearer ?token|refresh ?token|personal access token|private ?key|client ?secret|ssh ?key|pgp ?key|gpg ?key|signing ?key|webhook ?secret)\b/, level: 'never', kind: 'credential' },
    { re: /\b(recovery|backup) (codes?|keys?|phrases?)\b|\bseed (phrase|words)\b|\bmnemonic\b/, level: 'never', kind: 'credential' },
    { re: /\b(otp|one ?time (code|password|passcode|pin)|2fa|mfa|totp|verification code|auth(entication)? code|sms code|text(ed)? code|code (that )?(we|was|we've|we have)? ?(sent|texted|emailed)|code from (your )?(authenticator|app|sms|email|text)|[4-8] digit code)\b/, level: 'never', kind: 'one-time-code' },
    { re: /\b((security|secret|memorable|challenge) (question|answer|word|phrase)|maiden name|first pet|(city|town) (you were|where you were) born)\b/, level: 'never', kind: 'security-answer' },
    { re: /\b(card ?(number|no|num)|credit card (number|no)|debit card (number|no)|long number|number on (the )?(front|card)|cvv2?|cvc2?|cid|csc|card verification|security (digits|code)|3 digit (security )?(code|number)|three digit (security )?(code|number)|pan (number|no)|mm ?yy(yy)?|exp mm|valid (thru|through|until))\b/, level: 'never', kind: 'payment-card' },
    { re: /\b(credit ?card|debit ?card|exp(iry|iration)?( date| month| year)?)\b/, level: 'never', kind: 'payment-card', weak: true },
    { re: /\b(atm pin|card pin|pin (number|no))\b/, level: 'never', kind: 'pin' },
    { re: /\bpin\b/, level: 'never', kind: 'pin', pin: true },
    { re: /\b(secret|token)\b/, level: 'never', kind: 'credential', weak: true },
    { re: /\b(iban|international bank account( number| no)?|bank account( number| no)?|account (number|no|num)|acct (no|num|number)|routing (number|no)|aba (number|routing)|sort code|bsb|swift (code|bic)|bic|transit number|kontonummer|numer (konta|rachunku)|rib|clabe)\b/, level: 'confirm', kind: 'bank' },
    { re: /\b(ssn|social security( number| no)?|social insurance( number)?|national id( number)?|national identity|national insurance( number)?|ni number|nino|passport (number|no|num)|tax id|taxpayer id|tin|itin|ein|vat (number|id|no)|driver s? licen[cs]e|drivers licen[cs]e|driving licen[cs]e|personnummer|pesel|nip|regon|dow[oó]d|numer dowodu|steuer ?id|steueridentifikationsnummer|steuernummer|personalausweis|dni|nif|cpf|curp|rfc|codice fiscale|aadhaar|pan card|bsn|seguridad social|carte d identit[eé]|id card( number| no)?|identity (card|document|number))\b/, level: 'confirm', kind: 'government-id' },
    { re: /\bpassport\b/, level: 'confirm', kind: 'government-id', weak: true }
  ];
  function sensitivity(el, kind, label) {
    var type = lower(el.type);
    var tokens = lower(el.getAttribute('autocomplete')).split(' ');
    function has(tok) { return tokens.indexOf(tok) >= 0; }
    if (type === 'password' || has('current-password') || has('new-password')) return { level: 'never', kind: 'password' };
    if (has('one-time-code')) return { level: 'never', kind: 'one-time-code' };
    var NAME_TOKENS = { 'cc-name': 1, 'cc-given-name': 1, 'cc-family-name': 1, 'cc-additional-name': 1, 'cc-type': 1 };
    if (tokens.some(function (t) { return t.indexOf('cc-') === 0 && !NAME_TOKENS[t]; })) return { level: 'never', kind: 'payment-card' };
    var h = sensitiveHay(el, label);
    var head = headNoun(visibleLabelOf(el) || label);
    var postal = has('postal-code') || /\b(postal|zip|post ?code|postcode|india|area|locality|address)\b/.test(h.hay);
    for (var i = 0; i < RULES.length; i++) {
      var r = RULES[i];
      if (!r.re.test(h.hay)) continue;
      if (r.pin && postal) continue;
      if (r.weak && BENIGN_HEADS[head]) continue;
      return { level: r.level, kind: r.kind };
    }
    // Short national identifiers are only trusted in upper case ("SIN", not "sin").
    if (/\b(SIN|NIE|NIR)\b/.test(h.raw)) return { level: 'confirm', kind: 'government-id' };
    return null;
  }
  function luhn(d) {
    var s = 0, alt = false;
    for (var i = d.length - 1; i >= 0; i--) { var n = d.charCodeAt(i) - 48; if (alt) { n *= 2; if (n > 9) n -= 9; } s += n; alt = !alt; }
    return s % 10 === 0;
  }
  function ibanValid(v) {
    var s = v.replace(/\s/g, '').toUpperCase();
    if (!/^[A-Z]{2}\d{2}[A-Z0-9]{11,30}$/.test(s)) return false;
    var r = s.slice(4) + s.slice(0, 4), num = '';
    for (var i = 0; i < r.length; i++) { var c = r.charCodeAt(i); num += c >= 65 ? String(c - 55) : r[i]; }
    var rem = 0;
    for (var j = 0; j < num.length; j += 7) rem = parseInt(String(rem) + num.slice(j, j + 7), 10) % 97;
    return rem === 1;
  }
  var SECRET_VALUE = /-----BEGIN [A-Z ]*PRIVATE KEY-----|\b(sk|rk)_(live|test)_[A-Za-z0-9]{16,}|\bgh[pousr]_[A-Za-z0-9]{30,}|\bgithub_pat_[A-Za-z0-9_]{40,}|\bxox[abpors]-[A-Za-z0-9-]{10,}|\bAKIA[0-9A-Z]{16}\b|\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}|\bAIza[0-9A-Za-z_-]{35}\b/;
  // The value itself can be sensitive whatever the field claims to be.
  function valueSensitivity(value) {
    var v = String(value == null ? '' : value);
    if (SECRET_VALUE.test(v)) return { level: 'never', kind: 'credential-value' };
    var d = v.replace(/[\s-]/g, '');
    if (/^\d{13,19}$/.test(d) && /^(4|5[1-5]|2[2-7]|3[47]|6011|65|64[4-9]|35|62|36|30[0-5]|38|39)/.test(d) && luhn(d)) return { level: 'never', kind: 'payment-card-value' };
    if (ibanValid(v)) return { level: 'confirm', kind: 'bank-value' };
    return null;
  }

  // ---------------------------------------------------------------- visibility (anti "ghost field")
  function alpha(c) {
    if (!c || c === 'transparent') return 0;
    var m = /rgba?\(([^)]+)\)/.exec(c);
    if (!m) return 1;
    var parts = m[1].split(/[\s,\/]+/).filter(Boolean);
    if (parts.length >= 4) { var a = parts[3]; return a.indexOf('%') > 0 ? parseFloat(a) / 100 : parseFloat(a); }
    return 1;
  }
  function opacityOf(cs) {
    var op = parseFloat(cs.opacity);
    var fm = /opacity\(\s*([\d.]+)(%?)\s*\)/.exec(cs.filter || '');
    if (fm) op *= fm[2] ? parseFloat(fm[1]) / 100 : parseFloat(fm[1]);
    return op;
  }
  function effOpacity(n) {
    var op = 1;
    for (var x = n; x && x.nodeType === 1 && op >= 0.05; x = parentOf(x)) op *= opacityOf(getComputedStyle(x));
    return op;
  }
  function clipPathHides(cp) {
    return /^inset\(\s*(5\d|[6-9]\d|100)%/.test(cp) || /^circle\(\s*0(px|%)?[\s)]/.test(cp) ||
      /^ellipse\(\s*0(px|%)?\s/.test(cp) || /^polygon\((\s*0(px|%)?\s+0(px|%)?\s*,?)+\)$/.test(cp);
  }
  function maskHides(mi) {
    if (!mi || mi === 'none' || /url\(/.test(mi) || !/gradient\(/.test(mi)) return false;
    var cols = mi.match(/rgba?\([^)]*\)|transparent/g) || [];
    return cols.length > 0 && cols.every(function (c) { return alpha(c) === 0; });
  }
  function boxPaints(cs) { return alpha(cs.backgroundColor) >= 0.5 || (cs.backgroundImage && cs.backgroundImage !== 'none'); }
  function pseudoPaints(n) {
    var ps = ['::before', '::after'];
    for (var i = 0; i < 2; i++) {
      var s = getComputedStyle(n, ps[i]);
      if (s.content && s.content !== 'none' && s.content !== 'normal' && boxPaints(s)) return true;
    }
    return false;
  }
  function paints(n) {
    if (effOpacity(n) < 0.5) return false;
    if (/^(IMG|CANVAS|VIDEO|IFRAME|OBJECT|EMBED)$/.test(n.tagName) || n instanceof SVGElement) return true;
    return boxPaints(getComputedStyle(n)) || pseudoPaints(n);
  }
  // ---------------------------------------------------------------- can the person read the value?
  // A value the person cannot read cannot be reviewed before they submit, so it is not filled:
  // transparent text (a visible box does not help), text in the colour of what is behind it,
  // text too small to read, pushed out of the box, blurred or darkened by a filter, or painted
  // with a blend mode we cannot work out.
  var COLOR_CTX = null, COLOR_CACHE = new Map();
  // Any CSS colour as [r, g, b, a] (0-255, a 0-1). Colours that are not rgb() are drawn on a
  // 1x1 canvas and read back, so oklch(), lab() and color() cannot slip past the check.
  function rgbaOf(c) {
    if (!c || c === 'transparent') return [0, 0, 0, 0];
    if (COLOR_CACHE.has(c)) return COLOR_CACHE.get(c);
    var out = null;
    var m = /^rgba?\(([^)]+)\)$/.exec(String(c).trim());
    if (m) {
      var p = m[1].split(/[\s,\/]+/).filter(Boolean);
      out = [parseFloat(p[0]), parseFloat(p[1]), parseFloat(p[2]),
        p.length >= 4 ? (p[3].indexOf('%') > 0 ? parseFloat(p[3]) / 100 : parseFloat(p[3])) : 1];
    } else {
      try {
        if (!COLOR_CTX) { var cv = document.createElement('canvas'); cv.width = cv.height = 1; COLOR_CTX = cv.getContext('2d', { willReadFrequently: true }); }
        COLOR_CTX.clearRect(0, 0, 1, 1); COLOR_CTX.fillStyle = '#000'; COLOR_CTX.fillStyle = c; COLOR_CTX.fillRect(0, 0, 1, 1);
        var d = COLOR_CTX.getImageData(0, 0, 1, 1).data;
        out = [d[0], d[1], d[2], d[3] / 255];
      } catch (e) { out = null; }
    }
    COLOR_CACHE.set(c, out);
    return out;
  }
  function over(top, bottom) {
    var a = Math.max(0, Math.min(1, top[3]));
    return [top[0] * a + bottom[0] * (1 - a), top[1] * a + bottom[1] * (1 - a), top[2] * a + bottom[2] * (1 - a), 1];
  }
  function luminance(c) {
    var f = function (v) { v /= 255; return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]);
  }
  // Readable: enough luminance contrast, or clearly different colours (red on green).
  function distinct(a, b) {
    var la = luminance(a), lb = luminance(b);
    var ratio = (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
    var dr = a[0] - b[0], dg = a[1] - b[1], db = a[2] - b[2];
    return ratio >= 1.4 || Math.sqrt(dr * dr + dg * dg + db * db) >= 100;
  }
  var BLEND = {
    multiply: function (b, s) { return b * s; },
    screen: function (b, s) { return b + s - b * s; },
    darken: function (b, s) { return Math.min(b, s); },
    lighten: function (b, s) { return Math.max(b, s); },
    difference: function (b, s) { return Math.abs(b - s); },
    exclusion: function (b, s) { return b + s - 2 * b * s; },
    'hard-light': function (b, s) { return s <= 0.5 ? b * 2 * s : BLEND.screen(b, 2 * s - 1); },
    overlay: function (b, s) { return BLEND['hard-light'](s, b); },
    'color-dodge': function (b, s) { return b === 0 ? 0 : s >= 1 ? 1 : Math.min(1, b / (1 - s)); },
    'color-burn': function (b, s) { return b >= 1 ? 1 : s <= 0 ? 0 : 1 - Math.min(1, (1 - b) / s); }
  };
  function blendRGB(mode, backdrop, src) {
    var fn = BLEND[mode];
    if (!fn) return null;
    return [0, 1, 2].map(function (i) { return fn(backdrop[i] / 255, src[i] / 255) * 255; }).concat([1]);
  }
  // What paints behind a point of the field: the layers below it in the paint stack, over the
  // canvas. null when an image, a canvas or a video is involved: then we cannot tell.
  function backdropUnder(field, x, y) {
    var stack = withPointerEventsForced(field, function () { return stackAt(field, x, y); });
    var i = stack.indexOf(field);
    if (i < 0) return null;
    var layers = [];
    for (var j = i + 1; j < stack.length; j++) {
      var n = stack[j];
      if (n.id === HOST_ID || inOwnUi(n)) continue;
      if (/^(IMG|CANVAS|VIDEO|IFRAME|OBJECT|EMBED)$/.test(n.tagName) || n instanceof SVGElement) return null;
      var cs = getComputedStyle(n);
      if (cs.backgroundImage && cs.backgroundImage !== 'none') return null;
      var c = rgbaOf(cs.backgroundColor);
      if (!c) return null;
      if (c[3] > 0) { layers.push(c); if (c[3] >= 0.99) break; }
    }
    if (!layers.length && /dark/.test(getComputedStyle(document.documentElement).colorScheme || '')) return null;
    var bg = [255, 255, 255, 1];
    for (var k = layers.length - 1; k >= 0; k--) bg = over(layers[k], bg);
    return bg;
  }
  // null when the person can read text typed into the field (or when we cannot tell), else why not.
  function textHidden(field) {
    var cs = getComputedStyle(field);
    var r = field.getBoundingClientRect();
    if (parseFloat(cs.fontSize) < 6) return 'tiny-text';
    var indent = Math.abs(parseFloat(cs.textIndent) || 0);
    if (indent > 0 && indent >= r.width) return 'text-pushed-out';
    for (var n = field; n && n.nodeType === 1; n = parentOf(n)) {
      var flt = getComputedStyle(n).filter || '';
      if (flt === 'none') continue;
      var bm = /blur\(\s*([\d.]+)px/.exec(flt);
      if (bm && parseFloat(bm[1]) > 1.5) return 'blurred';
      var br = /brightness\(\s*([\d.]+)(%?)/.exec(flt);
      if (br && (br[2] ? parseFloat(br[1]) / 100 : parseFloat(br[1])) < 0.15) return 'darkened';
      var ct = /contrast\(\s*([\d.]+)(%?)/.exec(flt);
      if (ct && (ct[2] ? parseFloat(ct[1]) / 100 : parseFloat(ct[1])) < 0.2) return 'low-contrast';
    }
    var fill = rgbaOf(cs.webkitTextFillColor || cs.color);
    if (!fill) return null;
    var ink = fill;
    if (fill[3] < 0.1) {
      // Transparent text can still be drawn by the background (gradient text), a stroke or a
      // sharp shadow.
      if (/text/.test(cs.backgroundClip || cs.webkitBackgroundClip || '') && cs.backgroundImage !== 'none') return null;
      var sw = parseFloat(cs.webkitTextStrokeWidth) || 0, sc = rgbaOf(cs.webkitTextStrokeColor);
      var sh = /^(rgba?\([^)]*\))\s+(-?[\d.]+)px\s+(-?[\d.]+)px\s+([\d.]+)px/.exec(cs.textShadow || '');
      var shc = sh && rgbaOf(sh[1]);
      if (sw >= 0.5 && sc && sc[3] >= 0.5) ink = sc;
      else if (shc && shc[3] >= 0.5 && parseFloat(sh[4]) <= 3) ink = shc;
      else return 'invisible-text';
    }
    var cx = r.left + r.width / 2, cy = r.top + r.height / 2;
    if (!(cx >= 0 && cy >= 0 && cx < innerWidth && cy < innerHeight)) return null;
    var below = backdropUnder(field, cx, cy);
    if (!below || (cs.backgroundImage && cs.backgroundImage !== 'none')) return null;
    var own = rgbaOf(cs.backgroundColor);
    if (!own) return null;
    var paper = over(own, below);
    var text = over(ink, paper);
    // A blend mode on the field, or on a box around it, changes what reaches the screen.
    for (var b = field; b && b.nodeType === 1; b = parentOf(b)) {
      var mode = getComputedStyle(b).mixBlendMode;
      if (mode && mode !== 'normal') {
        var t2 = blendRGB(mode, below, text), p2 = blendRGB(mode, below, paper);
        if (!t2 || !p2) return 'blended';
        text = t2; paper = p2;
        break;
      }
    }
    return distinct(text, paper) ? null : 'same-colour-text';
  }
  var PE_SHEET = null;
  // Hit testing skips pointer-events:none, which is exactly how an overlay hides a field from
  // elementFromPoint. Force pointer-events on for the duration of the test, then restore.
  function withPointerEventsForced(field, fn) {
    if (PE_SHEET === null) {
      try { PE_SHEET = new CSSStyleSheet(); PE_SHEET.replaceSync('*, *::before, *::after { pointer-events: auto !important; }'); }
      catch (e) { PE_SHEET = false; }
    }
    var roots = [];
    if (PE_SHEET) {
      roots.push(document);
      var rn = field.getRootNode && field.getRootNode();
      if (rn && rn !== document && 'adoptedStyleSheets' in rn) roots.push(rn);
    }
    roots.forEach(function (r) { try { r.adoptedStyleSheets = r.adoptedStyleSheets.concat([PE_SHEET]); } catch (e) { /* ignore */ } });
    try { return fn(); } finally {
      roots.forEach(function (r) { try { r.adoptedStyleSheets = r.adoptedStyleSheets.filter(function (s) { return s !== PE_SHEET; }); } catch (e) { /* ignore */ } });
    }
  }
  function stackAt(field, x, y) {
    var rn = field.getRootNode && field.getRootNode();
    return (rn && rn !== document && rn.elementsFromPoint) ? rn.elementsFromPoint(x, y) : document.elementsFromPoint(x, y);
  }
  // Is the point covered? Walk the paint stack from the top until the field: anything above it
  // that paints (a background, an image, a pseudo-element box) covers it. Not finding the field
  // at all means it is not really there (clipped, flipped, moved away).
  function pointCovered(el, field, x, y) {
    var stack = stackAt(field, x, y);
    for (var i = 0; i < stack.length; i++) {
      var n = stack[i];
      if (n === el || n === field || el.contains(n) || field.contains(n)) return false;
      if (n.id === HOST_ID || inOwnUi(n)) continue;
      if (n.contains(field)) { if (pseudoPaints(n)) return true; continue; }
      if (paints(n)) return true;
    }
    return true;
  }
  function coverage(el, field, r) {
    var pts = [[0.5, 0.5], [0.25, 0.25], [0.75, 0.25], [0.25, 0.75], [0.75, 0.75]];
    var covered = 0, centre = false;
    withPointerEventsForced(field, function () {
      for (var i = 0; i < pts.length; i++) {
        var x = r.left + Math.max(2, Math.min(r.width - 2, r.width * pts[i][0]));
        var y = r.top + Math.max(2, Math.min(r.height - 2, r.height * pts[i][1]));
        if (x < 0 || y < 0 || x >= innerWidth || y >= innerHeight) continue;
        if (pointCovered(el, field, x, y)) { covered++; if (i === 0) centre = true; }
      }
    });
    return (centre || covered >= 3) ? 'covered' : null;
  }
  function isToggle(kind) { return kind === 'checkbox' || kind === 'radio' || kind === 'switch' || kind === 'radio-group'; }
  function sizeFloor(kind) {
    if (kind === 'text' || kind === 'datetime' || kind === 'select') return [10, 10];
    if (kind === 'contenteditable') return [1, 10];
    return [4, 4];
  }
  // A tiny control (react-select's growing input, a styled native checkbox) is shown through a
  // wrapper. Text fields never get this treatment: a tiny text input is the ghost-field trick.
  function visualTarget(el, kind) {
    if (kind !== 'combobox' && !isToggle(kind)) return el;
    var r = el.getBoundingClientRect();
    if (r.width >= 8 && r.height >= 8) return el;
    if (kind === 'combobox' && r.height < 8) return el;
    var node = el.parentElement;
    for (var i = 0; i < 5 && node; i++, node = node.parentElement) {
      var nr = node.getBoundingClientRect();
      if (nr.width >= 12 && nr.height >= 12) {
        var inside = r.left >= nr.left - 1 && r.right <= nr.right + 1 && r.top >= nr.top - 1 && r.bottom <= nr.bottom + 1;
        return (inside && node.querySelectorAll(FIELD_SELECTOR).length === 1) ? node : el;
      }
    }
    return el;
  }
  function isShown(n) {
    if (!n || !n.isConnected) return false;
    if (typeof n.checkVisibility === 'function' && !n.checkVisibility({ visibilityProperty: true, contentVisibilityAuto: true })) return false;
    var r = n.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  }
  // el: what to test (the field or its visual wrapper); field: the control itself.
  function visibilityOf(el, kind, field) {
    field = field || el;
    if (!el.isConnected || !field.isConnected) return { visible: false, reason: 'detached' };
    if (el.closest('[inert]') || field.closest('[inert]')) return { visible: false, reason: 'inert' };
    if (el.closest('[aria-hidden="true"]') || field.closest('[aria-hidden="true"]')) return { visible: false, reason: 'aria-hidden' };
    if (typeof el.checkVisibility === 'function' && !el.checkVisibility({ visibilityProperty: true, contentVisibilityAuto: true }))
      return { visible: false, reason: 'not-rendered' };
    var r = el.getBoundingClientRect();
    var floor = sizeFloor(kind);
    if (r.width < floor[0] || r.height < floor[1]) return { visible: false, reason: 'too-small' };
    var opacity = 1, fixed = false;
    for (var n = el; n && n.nodeType === 1; n = parentOf(n)) {
      var cs = getComputedStyle(n);
      opacity *= opacityOf(cs);
      if (cs.position === 'fixed') fixed = true;
      if (cs.clipPath && cs.clipPath !== 'none' && clipPathHides(cs.clipPath)) return { visible: false, reason: 'clipped' };
      if (cs.clip && cs.clip !== 'auto' && /rect\(\s*0(px)?[ ,]+0(px)?[ ,]+0(px)?[ ,]+0(px)?\s*\)/.test(cs.clip)) return { visible: false, reason: 'clipped' };
      if (maskHides(cs.maskImage || cs.webkitMaskImage)) return { visible: false, reason: 'masked' };
    }
    if (opacity < 0.5 && !isToggle(kind)) return { visible: false, reason: 'transparent' };
    if (opacity < 0.5 && isToggle(kind) && !visibleLabelOf(field)) return { visible: false, reason: 'transparent' };
    if (fixed) {
      if (r.right <= 0 || r.bottom <= 0 || r.left >= innerWidth || r.top >= innerHeight) return { visible: false, reason: 'off-screen' };
    } else {
      var de = document.documentElement;
      var docW = Math.max(de.scrollWidth, innerWidth), docH = Math.max(de.scrollHeight, innerHeight);
      var ax = r.left + scrollX, ay = r.top + scrollY;
      if (ax + r.width <= 0 || ay + r.height <= 0 || ax >= docW || ay >= docH) return { visible: false, reason: 'off-page' };
    }
    if (kind === 'text' || kind === 'datetime' || kind === 'contenteditable' || kind === 'select') {
      var th = textHidden(field);
      if (th) return { visible: false, reason: th };
    }
    var cx = r.left + r.width / 2, cy = r.top + r.height / 2;
    if (!(cx >= 0 && cy >= 0 && cx < innerWidth && cy < innerHeight)) return { visible: true, inViewport: false };
    var cov = coverage(el, field, r);
    if (cov) return { visible: false, reason: cov, inViewport: true };
    return { visible: true, inViewport: true };
  }

  // ---------------------------------------------------------------- submit-class controls and links
  function looksLikeSubmit(el) {
    if (!el || el.nodeType !== 1) return false;
    if (el.tagName === 'BUTTON' && el.type === 'submit' && el.form) return true;
    if (el.tagName === 'INPUT' && (el.type === 'submit' || el.type === 'image')) return true;
    var isButtonish = el.tagName === 'BUTTON' || el.tagName === 'A' || lower(el.getAttribute('role')) === 'button' ||
      (el.tagName === 'INPUT' && el.type === 'button');
    return isButtonish && SUBMIT_WORDS.test(norm(el.innerText || el.value || el.getAttribute('aria-label')));
  }
  // A link whose activation leaves this document.
  function navigatingLink(el) {
    var a = el.closest && el.closest('a[href], area[href]');
    if (!a) return null;
    var h = a.getAttribute('href') || '';
    if (/^\s*(#|javascript:)/i.test(h)) return null;
    try { if (new URL(a.href, location.href).href.split('#')[0] === location.href.split('#')[0]) return null; } catch (e) { /* treat as navigating */ }
    return a;
  }
  // Would a click on t (or on what t's activation forwards to) submit, save or navigate?
  function activatesSubmitClass(t) {
    if (!t || t.nodeType !== 1) return false;
    if (looksLikeSubmit(t) || navigatingLink(t)) return true;
    var lab = t.closest && t.closest('label');
    if (lab && lab.control && lab.control !== t) {
      var c = lab.control;
      if (c.tagName === 'BUTTON' || (c.tagName === 'INPUT' && /^(submit|image|reset|button)$/i.test(c.type))) return true;
    }
    var act = t.closest && t.closest('button, input[type=submit], input[type=image], [role=button]');
    return !!(act && act !== t && (looksLikeSubmit(act) || (act.tagName === 'BUTTON' && act.type === 'submit' && act.form)));
  }
  function submitControls() {
    var out = [], seen = 0;
    for (var el of allElements(document)) {
      if (seen > 3000) break;
      if (!(el.tagName === 'BUTTON' || el.tagName === 'INPUT' || el.tagName === 'A' || el.getAttribute('role') === 'button')) continue;
      seen++;
      if (inOwnUi(el) || !looksLikeSubmit(el) || !isShown(el)) continue;
      out.push({ ref: refFor(el), label: clip(accName(el) || el.innerText || el.value, 60),
        why: (el.type === 'submit' || el.type === 'image') ? 'type=' + el.type : 'label' });
      if (out.length >= 20) break;
    }
    return out;
  }

  // ---------------------------------------------------------------- describing fields
  function selectOptions(el) {
    return Array.prototype.slice.call(el.options, 0, 300).map(function (o) {
      return { label: norm(o.label || o.text), value: o.value, selected: o.selected, disabled: o.disabled };
    });
  }
  function rectOf(el) {
    var r = el.getBoundingClientRect();
    return { x: Math.round(r.left + scrollX), y: Math.round(r.top + scrollY), w: Math.round(r.width), h: Math.round(r.height) };
  }
  function sameText(a, b) { a = lower(a); b = lower(b); return !a || !b || a.indexOf(b) >= 0 || b.indexOf(a) >= 0; }
  function describe(el, kind) {
    var ref = refFor(el);
    var info = accNameInfo(el), label = info[0];
    var visible = visibleLabelOf(el);
    var vt = visualTarget(el, kind);
    var vis = visibilityOf(vt, kind, el);
    var sens = (kind === 'checkbox' || kind === 'switch') ? null : sensitivity(el, kind, label);
    var f = {
      ref: ref, kind: kind, tag: el.tagName.toLowerCase(),
      type: el.tagName === 'INPUT' ? lower(el.type) : null,
      role: el.getAttribute('role') || null,
      label: clip(label, 120), labelSource: info[1], visibleLabel: clip(visible, 120) || null,
      labelMismatch: (info[1] === 'aria-label' || info[1] === 'aria-labelledby') && !!visible && !sameText(visible, label),
      description: describedBy(el),
      placeholder: clip(el.getAttribute('placeholder'), 80) || null,
      name: el.getAttribute('name') || null, id: el.id || null,
      autocomplete: el.getAttribute('autocomplete') || null,
      required: !!(el.required || el.getAttribute('aria-required') === 'true'),
      disabled: !!(el.disabled || (el.matches && el.matches(':disabled')) || el.getAttribute('aria-disabled') === 'true'),
      readOnly: !!((el.readOnly && !isSelectOnlyInput(el)) || el.getAttribute('aria-readonly') === 'true'),
      visible: vis.visible, hiddenReason: vis.visible ? null : vis.reason, inViewport: !!vis.inViewport,
      sensitive: sens
    };
    if (kind === 'checkbox' || kind === 'switch') {
      f.checked = isChecked(el);
    } else {
      var v = currentValue(el, kind);
      f.hasValue = !!norm(v);
      f.value = sens ? (f.hasValue ? '•••' : '') : clip(v, 200);
    }
    if (kind === 'select') f.options = selectOptions(el);
    if (kind === 'combobox') f.optionsKnown = false;
    var userInvalid = false;
    try { userInvalid = el.matches(':user-invalid'); } catch (e) { /* older engines */ }
    if (el.getAttribute('aria-invalid') === 'true' || userInvalid) {
      f.invalid = true;
      f.validationMessage = clip(el.validationMessage || describedBy(el), 160) || null;
    }
    f.formRef = el.form ? refFor(el.form) : null;
    f.rect = rectOf(vt);
    // Identity, not state: options and "required" may change legitimately after another fill.
    f.fp = fnv([kind, f.type, f.name, f.id, f.autocomplete, f.label].join('␟'));
    return f;
  }

  function commonAncestor(nodes) {
    var a = nodes[0];
    while (a && !nodes.every(function (n) { return a.contains(n); })) a = a.parentElement;
    return a || document.body;
  }
  function groupMembers(container) {
    return Array.prototype.slice.call(container.querySelectorAll('input[type="radio"], [role="radio"]'));
  }
  function groupLabel(container, members) {
    var n = '';
    if (container.getAttribute && (container.getAttribute('aria-labelledby') || container.getAttribute('aria-label') || container.tagName === 'FIELDSET'))
      n = accName(container);
    if (!n) { var fs = members[0].closest('fieldset'); if (fs) n = accName(fs); }
    if (!n) n = nearbyText(container);
    return cleanLabel(n);
  }
  function describeGroup(key, container, members) {
    var ref = state.groupKeyRef.get(key);
    if (!ref) { ref = 'r' + (state.nextGroup++) + '_' + TAG; state.groupKeyRef.set(key, ref); }
    state.groups.set(ref, { key: key, container: new WeakRef(container), members: members.map(function (m) { return new WeakRef(m); }) });
    state.refs.set(ref, new WeakRef(container));
    var options = members.map(function (m) {
      return { label: clip(accName(m) || m.value, 80), value: m.value || null, selected: isChecked(m),
        disabled: !!(m.disabled || m.getAttribute('aria-disabled') === 'true') };
    });
    var vis = { visible: false, reason: 'no-visible-option' };
    for (var i = 0; i < members.length; i++) {
      var mv = visibilityOf(visualTarget(members[i], 'radio'), 'radio', members[i]);
      if (mv.visible) { vis = mv; break; }
      vis = mv;
    }
    var label = groupLabel(container, members);
    return {
      ref: ref, kind: 'radio-group', tag: container.tagName.toLowerCase(), type: null, role: container.getAttribute('role') || null,
      label: clip(label, 120), labelSource: 'group', visibleLabel: clip(label, 120) || null, labelMismatch: false,
      description: null, placeholder: null,
      name: members[0].getAttribute('name') || null, id: container.id || null, autocomplete: null,
      required: members.some(function (m) { return m.required; }) || container.getAttribute('aria-required') === 'true',
      disabled: members.every(function (m) { return m.disabled; }), readOnly: false,
      visible: vis.visible, hiddenReason: vis.visible ? null : vis.reason, inViewport: !!vis.inViewport,
      sensitive: null, options: options, hasValue: options.some(function (o) { return o.selected; }),
      value: (options.filter(function (o) { return o.selected; })[0] || {}).label || '',
      formRef: members[0].form ? refFor(members[0].form) : null, rect: rectOf(container),
      fp: fnv(['radio-group', label, options.map(function (o) { return o.label; }).join('|')].join('␟'))
    };
  }

  function scan() {
    var t0 = performance.now();
    var items = [];
    var nativeGroups = new Map();
    var index = 0;
    for (var el of allElements(document)) {
      index++;
      if (!el.matches(FIELD_SELECTOR) || inOwnUi(el)) continue;
      var kind = kindOf(el);
      if (!kind) continue;
      if (kind === 'radio') {
        if (el.closest('[role="radiogroup"]')) continue;
        var key = (el.form ? refFor(el.form) : 'doc') + '|' + (el.getAttribute('name') || refFor(el));
        var g = nativeGroups.get(key);
        if (!g) { g = { key: key, members: [], index: index }; nativeGroups.set(key, g); }
        g.members.push(el);
        continue;
      }
      if (kind === 'radio-group') {
        var ms = groupMembers(el);
        if (!ms.length) continue;
        items.push({ index: index, f: describeGroup('aria|' + refFor(el), el, ms) });
        continue;
      }
      if (kind === 'contenteditable' && el.parentElement && el.parentElement.closest(EDITABLE_SELECTOR)) continue;
      items.push({ index: index, f: describe(el, kind) });
    }
    nativeGroups.forEach(function (g) {
      var container = g.members[0].closest('fieldset') || commonAncestor(g.members);
      items.push({ index: g.index, f: describeGroup(g.key, container, g.members) });
    });
    items.sort(function (a, b) { return a.index - b.index; });
    var fields = items.map(function (x) { return x.f; });
    var frames = Array.prototype.slice.call(document.querySelectorAll('iframe'), 0, 20).map(function (fr) {
      var same = false, origin = null;
      try { same = !!fr.contentDocument; } catch (e) { same = false; }
      try { origin = new URL(fr.src || 'about:blank', location.href).origin; } catch (e) { origin = null; }
      return { origin: origin, sameOrigin: same, visible: isShown(fr) };
    });
    var counts = { total: fields.length, visible: 0, hidden: {}, sensitive: {} };
    fields.forEach(function (f) {
      if (f.visible) counts.visible++; else counts.hidden[f.hiddenReason] = (counts.hidden[f.hiddenReason] || 0) + 1;
      if (f.sensitive) counts.sensitive[f.sensitive.level] = (counts.sensitive[f.sensitive.level] || 0) + 1;
    });
    return {
      docId: DOC_ID, url: location.href, origin: location.origin, path: location.pathname, title: document.title,
      fields: fields, submitControls: submitControls(), frames: frames, counts: counts,
      ms: Math.round((performance.now() - t0) * 10) / 10
    };
  }

  function fieldFor(ref) {
    var g = state.groups.get(ref);
    if (g) {
      var c = g.container.deref();
      var ms = g.members.map(function (w) { return w.deref(); }).filter(function (m) { return m && m.isConnected; });
      if (!c || !c.isConnected || !ms.length) return null;
      return { el: c, kind: 'radio-group', members: ms, f: describeGroup(g.key, c, ms) };
    }
    var el = byRef(ref);
    if (!el) return null;
    var kind = kindOf(el);
    if (!kind) return null;
    return { el: el, kind: kind, f: describe(el, kind) };
  }

  // ---------------------------------------------------------------- page UI: outline + indicator + Stop
  var CSS = [
    '.outline{position:fixed;display:none;border:2px solid #6d5dfc;border-radius:6px;box-shadow:0 0 0 4px rgba(109,93,252,.25);pointer-events:none;transition:left .12s,top .12s,width .12s,height .12s}',
    '.pill{position:fixed;top:12px;right:12px;display:flex;gap:10px;align-items:center;background:#1d1b29;color:#fff;font:13px/1.3 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;padding:8px 8px 8px 12px;border-radius:999px;box-shadow:0 6px 20px rgba(0,0,0,.28);pointer-events:auto}',
    '.dot{width:8px;height:8px;border-radius:50%;background:#8b7dff;animation:pulse 1.2s infinite}',
    '@keyframes pulse{50%{opacity:.3}}',
    'button{all:unset;cursor:pointer;background:#fff;color:#1d1b29;border-radius:999px;padding:4px 12px;font-weight:600}',
    'button:focus-visible{outline:2px solid #8b7dff;outline-offset:2px}'
  ].join('');
  function ensureUi() {
    if (state.host && state.host.isConnected) return;
    var host = document.createElement('div');
    host.id = HOST_ID;
    host.style.cssText = 'all:initial;position:fixed;left:0;top:0;width:0;height:0;z-index:2147483647;pointer-events:none';
    var shadow = host.attachShadow({ mode: 'closed' });
    try {
      var sheet = new CSSStyleSheet();
      sheet.replaceSync(CSS);
      shadow.adoptedStyleSheets = [sheet];
    } catch (e) { /* CSSOM unavailable: unstyled but still works */ }
    var outline = document.createElement('div'); outline.className = 'outline';
    var pill = document.createElement('div'); pill.className = 'pill';
    pill.setAttribute('role', 'status');
    var dot = document.createElement('span'); dot.className = 'dot';
    var msg = document.createElement('span'); msg.textContent = 'AI Expedite is filling this form';
    var btn = document.createElement('button'); btn.type = 'button'; btn.textContent = 'Stop';
    btn.addEventListener('click', function (e) {
      if (!state.stopRequested) { state.stopRequested = true; state.stopAt = Date.now(); state.stopTrusted = e.isTrusted; }
      msg.textContent = 'Stopping…';
    });
    pill.appendChild(dot); pill.appendChild(msg); pill.appendChild(btn);
    shadow.appendChild(outline); shadow.appendChild(pill);
    (document.body || document.documentElement).appendChild(host);
    state.host = host; state.outline = outline; state.msg = msg;
  }
  function track() {
    state.raf = 0;
    var t = state.outlineTarget && state.outlineTarget.deref();
    if (!state.outline) return;
    if (!t || !t.isConnected) { state.outline.style.display = 'none'; return; }
    var r = t.getBoundingClientRect(), s = state.outline.style;
    s.display = 'block'; s.left = (r.left - 4) + 'px'; s.top = (r.top - 4) + 'px';
    s.width = (r.width + 8) + 'px'; s.height = (r.height + 8) + 'px';
    state.raf = requestAnimationFrame(track);
  }
  function highlight(el) {
    ensureUi();
    state.outlineTarget = new WeakRef(el);
    if (state.raf) cancelAnimationFrame(state.raf);
    track();
  }
  function clearHighlight() {
    state.outlineTarget = null;
    if (state.raf) cancelAnimationFrame(state.raf);
    state.raf = 0;
    if (state.outline) state.outline.style.display = 'none';
  }
  function say(text) { if (state.msg) state.msg.textContent = text; }

  // ---------------------------------------------------------------- guards against submitting
  // The guards run in three modes. 'fill': a fill window (the outline and the Stop pill show).
  // 'click': the agent clicks one link or non-submit control; only the navigation that click
  // asked for goes through. 'submit': the person approved one submit; exactly that control's
  // submission goes through, once. In every mode the page's own submits and navigations are
  // cancelled, and a click or key press by the person goes through (except in 'submit' mode,
  // where nothing but the approved submission may leave the page).
  var AGENT_ACT_MS = 2000;      // a trusted click the agent sends is not the person acting
  var APPROVED_MS = 8000;       // an approved submit must happen within this
  function guarding() { return state.active || Date.now() < state.guardUntil; }
  function agentActing() { return Date.now() < state.agentActingUntil; }
  function personJustActed() { return Date.now() - state.lastGestureAt < 1000; }
  function onGesture(e) { if (e.isTrusted && !inOwnUi(e.target) && !agentActing()) state.lastGestureAt = Date.now(); }
  function phase() { return state.active ? (state.mode === 'fill' ? 'during-fill' : 'during-' + state.mode) : 'after-' + (state.mode || 'fill'); }
  function record(kind, detail) {
    var entry = { at: Date.now(), phase: phase(), kind: kind };
    for (var k in detail) entry[k] = detail[k];
    if (state.blockedSubmits.length < 50) state.blockedSubmits.push(entry);
    if (state.mode === 'fill') say(state.active ? 'Blocked the page from submitting while filling' : 'Blocked the page from submitting on its own');
  }
  function sameUrl(a, b) {
    try { return new URL(a, location.href).href.split('#')[0] === new URL(b, location.href).href.split('#')[0]; } catch (e) { return false; }
  }
  // The submission the person approved: its control (the submitter) or, for a control that
  // submits its form without being the submitter, that form. Used once.
  function approvedSubmit(e) {
    var a = state.approved;
    if (!a || a.used || Date.now() > a.until) return false;
    var el = a.el.deref(), form = a.form && a.form.deref();
    if (!el) return false;
    if (e.submitter === el || (!e.submitter && form && e.target === form) || (a.virtual && !e.submitter)) {
      a.used = true;
      return true;
    }
    return false;
  }
  // After the approved submission (or the approved click on a control outside any form), the
  // page may navigate: to where the form posts (once), or anywhere without form data (a
  // script-driven page that saved by fetch and then shows its "thank you" page).
  //
  // A control whose onclick calls form.submit() fires no submit event: once the approved control
  // was pressed, the approved form's own post (to its own action, once) goes through as well, and
  // so does a navigation without form data (a script that saved and shows its confirmation).
  function approvedNavigation(e) {
    var a = state.approved;
    if (!a || Date.now() > a.until + APPROVED_MS) return false;
    if (!(a.used || a.virtual || a.pressed)) return false;
    if (!e.formData) return true;
    if (a.navUsed) return false;
    if (!(a.used || a.virtual)) {
      var dest = e.destination && e.destination.url;
      if (!dest || a.actions.indexOf(String(dest).split('#')[0]) < 0) return false;
      a.used = true;
    }
    a.navUsed = true;
    return true;
  }
  // The approved control was pressed (the agent's trusted click, or its click() on the AppleScript
  // route). Only that unlocks the script-driven submission above.
  function onClickCapture(e) {
    var a = state.approved;
    if (!a || state.mode !== 'submit' || Date.now() > a.until) return;
    var el = a.el.deref();
    if (el && e.target && (e.target === el || composedContains(el, e.target))) a.pressed = true;
  }
  function onSubmitCapture(e) {
    var s = e.submitter;
    if (approvedSubmit(e)) {
      state.allowedByAgent.push({ at: Date.now(), kind: 'approved-submit' });
      return;
    }
    if (!guarding()) return;
    if (state.mode !== 'submit' && !state.active && personJustActed()) {
      state.allowedByPerson.push({ at: Date.now(), kind: 'submit', submitter: s ? clip(accName(s) || s.innerText || s.value, 60) : null });
      return;
    }
    e.preventDefault();
    e.stopImmediatePropagation();
    record('submit-event', { trusted: e.isTrusted, submitter: s ? clip(accName(s) || s.innerText || s.value, 60) : null });
  }
  // The browser's own validation refused the approved submission: it fires `invalid` on the
  // offending fields and no submit event. That is the one proof that nothing was sent.
  function onInvalidCapture() {
    if (state.mode === 'submit' && state.active) state.sawInvalid = true;
  }
  // form.submit() and location.assign() fire no submit event; the Navigation API sees both.
  function onNavigate(e) {
    if (approvedNavigation(e)) return;
    if (!guarding()) return;
    var dest = '';
    try { dest = new URL(e.destination.url).pathname; } catch (x) { dest = ''; }
    if (e.destination && e.destination.sameDocument && !e.formData) return;   // pushState/hash: no data leaves
    if (state.mode === 'click' && !e.formData && state.allowNavHref && e.destination && sameUrl(e.destination.url, state.allowNavHref)) {
      state.allowedByAgent.push({ at: Date.now(), kind: 'link', to: dest });
      return;
    }
    if (state.mode !== 'submit' && ((e.userInitiated && !(agentActing() && e.formData)) || (!state.active && personJustActed()))) {
      state.allowedByPerson.push({ at: Date.now(), kind: 'navigation', to: dest });
      return;
    }
    if (e.cancelable) e.preventDefault();
    record(e.formData ? 'form-submission' : 'navigation', { to: dest, cancelled: !!e.cancelable, navigationType: e.navigationType });
  }
  // A form that targets another browsing context (a hidden iframe) would post without this
  // document navigating. Point such forms at the page itself while guarding, so the navigation
  // guard sees them; put the attribute back exactly afterwards.
  function retargetForms() {
    var baseTarget = (document.querySelector('base[target]') || {}).target || '';
    Array.prototype.forEach.call(document.forms, function (form) {
      var own = form.getAttribute('target');
      var eff = own || baseTarget;
      if (!eff || /^(_self|_top|_parent)$/i.test(eff)) return;
      state.retargeted.push({ form: new WeakRef(form), had: form.hasAttribute('target'), value: own });
      form.setAttribute('target', '_self');
    });
  }
  function restoreForms() {
    state.retargeted.forEach(function (r) {
      var f = r.form.deref();
      if (!f || f.getAttribute('target') !== '_self') return;
      if (r.had) f.setAttribute('target', r.value); else f.removeAttribute('target');
    });
    state.retargeted = [];
  }
  function installGuards() {
    if (state.teardownTimer) { clearTimeout(state.teardownTimer); state.teardownTimer = 0; }
    if (state.guardsInstalled) return;
    window.addEventListener('submit', onSubmitCapture, true);
    window.addEventListener('invalid', onInvalidCapture, true);
    window.addEventListener('click', onClickCapture, true);
    window.addEventListener('pointerdown', onGesture, true);
    window.addEventListener('keydown', onGesture, true);
    if (globalThis.navigation && navigation.addEventListener) navigation.addEventListener('navigate', onNavigate);
    retargetForms();
    try {
      state.perf = new PerformanceObserver(function (list) {
        list.getEntries().forEach(function (e) {
          if (!/^(fetch|xmlhttprequest|beacon|other)$/.test(e.initiatorType) || state.requests.length >= 50) return;
          var u = '';
          try { var p = new URL(e.name); u = p.origin + p.pathname; } catch (x) { u = ''; }
          state.requests.push({ type: e.initiatorType, url: u, phase: phase() });
        });
      });
      state.perf.observe({ type: 'resource' });
    } catch (e) { state.perf = null; }
    state.guardsInstalled = true;
  }
  function teardownGuards() {
    state.teardownTimer = 0;
    if (!state.guardsInstalled) return;
    window.removeEventListener('submit', onSubmitCapture, true);
    window.removeEventListener('invalid', onInvalidCapture, true);
    window.removeEventListener('click', onClickCapture, true);
    window.removeEventListener('pointerdown', onGesture, true);
    window.removeEventListener('keydown', onGesture, true);
    if (globalThis.navigation && navigation.removeEventListener) navigation.removeEventListener('navigate', onNavigate);
    restoreForms();
    if (state.perf) { try { state.perf.disconnect(); } catch (e) { /* ignore */ } state.perf = null; }
    state.guardsInstalled = false;
  }
  // Starts a guard window in a mode. The fill window keeps its own begin()/end() (UI, Stop).
  function openGuard(mode) {
    if (!state.watchdog) state.watchdog = setInterval(checkAgent, 2000);
    state.mode = mode; state.active = true; state.guardUntil = 0;
    state.blockedSubmits = []; state.allowedByPerson = []; state.allowedByAgent = []; state.requests = [];
    state.sawInvalid = false;
    installGuards();
  }
  function closeGuard(graceMs) {
    state.active = false;
    state.agentActingUntil = 0;
    if (state.watchdog) { clearInterval(state.watchdog); state.watchdog = 0; }
    state.guardUntil = Date.now() + graceMs;
    if (state.teardownTimer) clearTimeout(state.teardownTimer);
    state.teardownTimer = setTimeout(teardownGuards, graceMs + 100);
  }

  // ---------------------------------------------------------------- filling
  function setNativeValue(el, value) {
    var proto = el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : el.tagName === 'SELECT' ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, value);
  }
  function fireInput(el, data) {
    el.dispatchEvent(new InputEvent('input', { bubbles: true, composed: true, inputType: 'insertText', data: data }));
  }
  function fireChange(el) { el.dispatchEvent(new Event('change', { bubbles: true })); }
  function isFocused(el) { var r = el.getRootNode(); return (r && r.activeElement === el) || document.activeElement === el; }

  // Text: focus, select what's there, insert through Blink's editing pipeline (trusted input
  // events, like typing). If that inserted nothing, fall back to the native setter plus events.
  // Never paste over a value the page reformatted: verify decides whether it is right.
  function fillText(el, value) {
    if (el.tagName === 'INPUT') value = String(value).replace(/[\r\n]+/g, ' ');
    el.focus({ preventScroll: true });
    if (!isFocused(el)) {
      setNativeValue(el, value); fireInput(el, value); fireChange(el);
      return 'setter (field could not take focus)';
    }
    var before = el.value;
    try { if (typeof el.select === 'function') el.select(); else document.execCommand('selectAll'); } catch (e) {
      try { document.execCommand('selectAll'); } catch (e2) { /* ignore */ }
    }
    var ok = false;
    try { ok = value === '' ? document.execCommand('delete') : document.execCommand('insertText', false, value); } catch (e) { ok = false; }
    if (ok && (el.value !== before || el.value === value)) return 'execCommand';
    setNativeValue(el, value); fireInput(el, value); fireChange(el);
    return ok ? 'setter (execCommand changed nothing)' : 'setter';
  }
  function selectContents(el) {
    el.focus({ preventScroll: true });
    var range = document.createRange(); range.selectNodeContents(el);
    var sel = getSelection(); sel.removeAllRanges(); sel.addRange(range);
  }
  // Model-backed editors (Slate, Draft, Lexical) listen for beforeinput, which execCommand
  // does not fire: give them the chance to apply the text themselves first.
  function fillContentEditable(el, value) {
    selectContents(el);
    var bi = new InputEvent('beforeinput', { inputType: 'insertReplacementText', data: value, bubbles: true, cancelable: true, composed: true });
    if (!el.dispatchEvent(bi)) return 'beforeinput (handled by the editor)';
    var ok = false;
    try { ok = document.execCommand('insertText', false, value); } catch (e) { ok = false; }
    return ok ? 'execCommand' : 'failed';
  }
  function pointerSequence(target) {
    var r = target.getBoundingClientRect();
    var x = r.left + Math.min(r.width / 2, 40), y = r.top + r.height / 2;
    var base = { bubbles: true, cancelable: true, composed: true, clientX: x, clientY: y, screenX: x, screenY: y, view: window };
    var pbase = Object.assign({ pointerId: 1, pointerType: 'mouse', isPrimary: true }, base);
    target.dispatchEvent(new PointerEvent('pointerover', Object.assign({}, pbase, { buttons: 0 })));
    target.dispatchEvent(new MouseEvent('mouseover', Object.assign({}, base, { buttons: 0 })));
    target.dispatchEvent(new PointerEvent('pointerdown', Object.assign({}, pbase, { button: 0, buttons: 1 })));
    target.dispatchEvent(new MouseEvent('mousedown', Object.assign({}, base, { button: 0, buttons: 1 })));
    if (typeof target.focus === 'function') target.focus({ preventScroll: true });
    target.dispatchEvent(new PointerEvent('pointerup', Object.assign({}, pbase, { button: 0, buttons: 0 })));
    target.dispatchEvent(new MouseEvent('mouseup', Object.assign({}, base, { button: 0, buttons: 0 })));
    // A click activates: never send one whose activation reaches a submit-class control or a link.
    if (!activatesSubmitClass(target)) target.dispatchEvent(new MouseEvent('click', Object.assign({}, base, { button: 0, buttons: 0 })));
  }
  function clickToggle(el) {
    if (activatesSubmitClass(el)) return false;
    if (el.tagName === 'INPUT') el.click(); else pointerSequence(el);
    return true;
  }
  function keyOpen(el) {
    // ArrowDown opens most listbox comboboxes. Never Enter: in a form that can submit.
    el.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', code: 'ArrowDown', keyCode: 40, which: 40, bubbles: true, cancelable: true, composed: true }));
  }
  function toBool(v) { return ['true', 'yes', 'y', 'on', '1', 'checked', 'agree', 'accept', 'tick', 'ticked'].indexOf(lower(v)) >= 0; }
  function matchOption(options, value) {
    var v = lower(value);
    if (!v) return { match: null };
    var tests = [
      function (o) { return lower(o.label) === v; },
      function (o) { return o.value != null && lower(o.value) === v; },
      function (o) { return lower(o.label).indexOf(v) === 0; },
      function (o) { return lower(o.label).indexOf(v) >= 0; }
    ];
    for (var i = 0; i < tests.length; i++) {
      var m = options.filter(function (o) { return !o.disabled && tests[i](o); });
      if (m.length === 1) return { match: m[0] };
      if (m.length > 1) return { ambiguous: m.map(function (o) { return o.label; }).slice(0, 20) };
    }
    return { match: null };
  }
  function findListbox(el) {
    var ids = norm([el.getAttribute('aria-controls'), el.getAttribute('aria-owns')].join(' ')).split(' ').filter(Boolean);
    for (var i = 0; i < ids.length; i++) {
      var n = root(el).getElementById(ids[i]) || document.getElementById(ids[i]);
      if (n && isShown(n)) return n.matches('[role="listbox"]') ? n : (n.querySelector('[role="listbox"]') || n);
    }
    var all = Array.prototype.filter.call(document.querySelectorAll('[role="listbox"]'), function (n) { return !inOwnUi(n) && isShown(n) && !n.matches('select'); });
    return all.length ? all[all.length - 1] : null;
  }
  function listOptions(lb) {
    return Array.prototype.filter.call(lb.querySelectorAll('[role="option"]'), isShown).map(function (o) {
      return { el: o, label: norm(o.innerText || o.textContent), disabled: o.getAttribute('aria-disabled') === 'true' };
    });
  }
  // Menus without ARIA roles: look only inside nodes that appeared after we opened the control.
  function fallbackOptions(p) {
    var out = [], seen = new Set();
    p.added.forEach(function (w) {
      var n = w.deref();
      if (!n || !n.isConnected || n.nodeType !== 1) return;
      var cands = [n].concat(Array.prototype.slice.call(n.querySelectorAll('div,li,span,a'), 0, 400));
      cands.forEach(function (c) {
        if (seen.has(c) || !isShown(c)) return;
        var leafish = c.children.length === 0 || /option/i.test(c.className || '') || /option/i.test(c.id || '');
        var t = norm(c.innerText);
        if (leafish && t && t.length <= 80) { seen.add(c); out.push({ el: c, label: t, disabled: false }); }
      });
    });
    return out;
  }
  function stopPending(ref) {
    var p = state.pending.get(ref);
    if (p && p.observer) p.observer.disconnect();
    state.pending.delete(ref);
  }
  function dismiss(el) { if (isFocused(el)) el.blur(); }   // never Escape: it can close the page's own dialog
  // Check again right before the value lands: a page can hide a field once it is focused.
  function recheck(target, kind, field) {
    var v = visibilityOf(visualTarget(target, kind), kind, field || target);
    if (!v.visible) return v.reason + ' (changed before the value was written)';
    if (!v.inViewport) return 'off-screen (changed before the value was written)';
    return null;
  }

  function fillStep(ref, value, step, opts) {
    opts = opts || {};
    var info = fieldFor(ref);
    if (!info) { stopPending(ref); return { done: true, outcome: 'not-found' }; }
    var el = info.el, kind = info.kind, f = info.f;
    // Stop wins over anything not written yet: a click on Stop while this field is highlighted,
    // or while a dropdown is loading, leaves this field alone too. (Verify only reads.)
    if (state.stopRequested && step !== 'verify') {
      stopPending(ref);
      if (step === 'pick') dismiss(el);
      return { done: true, outcome: 'stopped-mid-field' };
    }
    if (step === 'start') {
      var vs = valueSensitivity(value);
      if (vs && vs.level === 'never') return { done: true, outcome: 'refused-sensitive', detail: vs.kind };
      if (vs && vs.level === 'confirm' && !opts.sensitiveConfirmed) return { done: true, outcome: 'needs-confirmation', detail: vs.kind };
      if (f.sensitive && f.sensitive.level === 'never') return { done: true, outcome: 'refused-sensitive', detail: f.sensitive.kind };
      if (f.sensitive && f.sensitive.level === 'confirm' && !opts.sensitiveConfirmed) return { done: true, outcome: 'needs-confirmation', detail: f.sensitive.kind };
      if (f.disabled || f.readOnly) return { done: true, outcome: 'not-editable' };
      if (kind === 'unsupported') return { done: true, outcome: 'unsupported', detail: f.type || f.tag };
      var target = el, vkind = kind;
      if (kind === 'radio-group') {
        var mm = matchOption(f.options.map(function (o, i) { return { label: o.label, value: o.value, disabled: o.disabled, i: i }; }), value);
        if (mm.ambiguous) return { done: true, outcome: 'ambiguous-option', options: mm.ambiguous };
        if (!mm.match) return { done: true, outcome: 'no-matching-option', options: f.options.map(function (o) { return o.label; }) };
        target = info.members[mm.match.i];
        vkind = 'radio';
      }
      var vt = visualTarget(target, vkind);
      vt.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
      var vis = visibilityOf(vt, vkind, target);
      if (!vis.visible) {
        if (vis.reason === 'covered') {
          var pc = state.pending.get(ref) || {};
          if (!pc.coveredSince) pc.coveredSince = Date.now();
          state.pending.set(ref, pc);
          if (Date.now() - pc.coveredSince < COVER_RETRY_MS) return { done: false, next: 'start', waitMs: 150 };
        }
        stopPending(ref);
        return { done: true, outcome: 'refused-hidden', detail: vis.reason };
      }
      // Scrolling to the centre always brings a reachable field into view.
      if (!vis.inViewport) { stopPending(ref); return { done: true, outcome: 'refused-hidden', detail: 'off-screen' }; }
      stopPending(ref);
      highlight(kind === 'radio-group' ? (el.closest('fieldset') || el) : vt);
      return { done: false, next: 'apply', waitMs: opts.highlightMs == null ? 300 : opts.highlightMs };
    }
    if (step === 'apply') {
      if (kind === 'text') {
        el.focus({ preventScroll: true });
        var why = recheck(el, kind);
        if (why) return { done: true, outcome: 'refused-hidden', detail: why };
        if (opts.trustedText) {
          try { if (typeof el.select === 'function') el.select(); else document.execCommand('selectAll'); } catch (e) { document.execCommand('selectAll'); }
          if (!isFocused(el)) return { done: true, outcome: 'failed', detail: 'field did not take focus for trusted typing' };
          return { done: false, next: 'verify', waitMs: 120, insertText: el.tagName === 'INPUT' ? String(value).replace(/[\r\n]+/g, ' ') : value };
        }
        return { done: false, next: 'verify', waitMs: 120, method: fillText(el, value) };
      }
      if (kind === 'datetime') {
        el.focus({ preventScroll: true });
        var why2 = recheck(el, kind);
        if (why2) return { done: true, outcome: 'refused-hidden', detail: why2 };
        setNativeValue(el, value); fireInput(el, value); fireChange(el);
        return { done: false, next: 'verify', waitMs: 80, method: 'setter' };
      }
      if (kind === 'select') {
        var opts2 = Array.prototype.map.call(el.options, function (o) { return { label: norm(o.label || o.text), value: o.value, disabled: o.disabled }; });
        var m = matchOption(opts2, value);
        if (m.ambiguous) return { done: true, outcome: 'ambiguous-option', options: m.ambiguous };
        if (!m.match) {
          // Dependent selects (state after country) load late: keep waiting up to the limit.
          var p0 = state.pending.get(ref);
          if (!p0 || !p0.started) { p0 = { started: Date.now() }; state.pending.set(ref, p0); }
          if (Date.now() - p0.started < COMBO_WAIT_MS && opts2.length <= 1) return { done: false, next: 'apply', waitMs: 300 };
          stopPending(ref);
          return { done: true, outcome: 'no-matching-option', options: opts2.map(function (o) { return o.label; }).slice(0, 25) };
        }
        stopPending(ref);
        el.focus({ preventScroll: true });
        var why3 = recheck(el, kind);
        if (why3) return { done: true, outcome: 'refused-hidden', detail: why3 };
        setNativeValue(el, m.match.value); fireInput(el, m.match.label); fireChange(el);
        return { done: false, next: 'verify', waitMs: 80, method: 'setter', chosen: m.match.label };
      }
      if (kind === 'checkbox' || kind === 'switch') {
        var want = toBool(value);
        if (isChecked(el) === want) return { done: true, outcome: 'already-set' };
        var why4 = recheck(el, kind);
        if (why4) return { done: true, outcome: 'refused-hidden', detail: why4 };
        if (!clickToggle(el)) return { done: true, outcome: 'refused-submit-class' };
        return { done: false, next: 'verify', waitMs: 80, method: el.tagName === 'INPUT' ? 'click' : 'pointer' };
      }
      if (kind === 'radio-group') {
        var mo = matchOption(f.options.map(function (o, i) { return { label: o.label, value: o.value, disabled: o.disabled, i: i }; }), value);
        if (!mo.match) return { done: true, outcome: 'no-matching-option' };
        var member = info.members[mo.match.i];
        if (isChecked(member)) return { done: true, outcome: 'already-set' };
        var why5 = recheck(member, 'radio');
        if (why5) return { done: true, outcome: 'refused-hidden', detail: why5 };
        if (!clickToggle(member)) return { done: true, outcome: 'refused-submit-class' };
        return { done: false, next: 'verify', waitMs: 80, method: 'click', chosen: mo.match.label };
      }
      if (kind === 'combobox') {
        stopPending(ref);
        var why6 = recheck(el, kind);
        if (why6) return { done: true, outcome: 'refused-hidden', detail: why6 };
        var p = { started: Date.now(), attempts: 0, added: [], observer: null };
        p.observer = new MutationObserver(function (muts) {
          for (var i = 0; i < muts.length; i++) for (var j = 0; j < muts[i].addedNodes.length; j++) {
            var n = muts[i].addedNodes[j];
            if (n.nodeType === 1 && !inOwnUi(n) && p.added.length < 300) p.added.push(new WeakRef(n));
          }
        });
        p.observer.observe(document.documentElement, { childList: true, subtree: true });
        state.pending.set(ref, p);
        if (el.tagName === 'INPUT' && !isSelectOnlyInput(el)) {
          pointerSequence(visualTarget(el, kind));
          el.focus({ preventScroll: true });
          var method = fillText(el, value);   // typing filters the options and opens the list
          return { done: false, next: 'pick', waitMs: 350, method: 'type-to-filter (' + method + ')' };
        }
        pointerSequence(visualTarget(el, kind));
        return { done: false, next: 'pick', waitMs: 300, method: 'pointer-open' };
      }
      if (kind === 'contenteditable') {
        var why7 = recheck(el, kind);
        if (why7) return { done: true, outcome: 'refused-hidden', detail: why7 };
        if (opts.trustedText) {
          selectContents(el);
          if (!isFocused(el) && !el.contains(document.activeElement)) return { done: true, outcome: 'failed', detail: 'editor did not take focus' };
          return { done: false, next: 'verify', waitMs: 250, insertText: value };
        }
        return { done: false, next: 'verify', waitMs: 250, method: fillContentEditable(el, value) };
      }
      return { done: true, outcome: 'unsupported', detail: kind };
    }
    if (step === 'pick') {
      var pp = state.pending.get(ref);
      if (!pp) return { done: true, outcome: 'failed', detail: 'lost dropdown state' };
      pp.attempts++;
      var lb = findListbox(el);
      var options = lb ? listOptions(lb) : [];
      if (!options.length) options = fallbackOptions(pp);
      if (!options.length) {
        if (Date.now() - pp.started < COMBO_WAIT_MS) {
          if (pp.attempts === 3 && (el.tagName !== 'INPUT' || isSelectOnlyInput(el))) keyOpen(el);
          return { done: false, next: 'pick', waitMs: 250 };
        }
        stopPending(ref); dismiss(el);
        return { done: true, outcome: 'no-options', detail: 'the dropdown list did not appear' };
      }
      var mc = matchOption(options, value);
      if (mc.ambiguous) { stopPending(ref); dismiss(el); return { done: true, outcome: 'ambiguous-option', options: mc.ambiguous }; }
      if (!mc.match) {
        if (Date.now() - pp.started < 2500) return { done: false, next: 'pick', waitMs: 250 };
        stopPending(ref); dismiss(el);
        return { done: true, outcome: 'no-matching-option', options: options.map(function (o) { return o.label; }).slice(0, 25) };
      }
      if (navigatingLink(mc.match.el)) { stopPending(ref); dismiss(el); return { done: true, outcome: 'refused-navigating-option', chosen: mc.match.label }; }
      if (activatesSubmitClass(mc.match.el)) { stopPending(ref); dismiss(el); return { done: true, outcome: 'refused-submit-class', chosen: mc.match.label }; }
      mc.match.el.scrollIntoView({ block: 'nearest', behavior: 'instant' });
      pointerSequence(mc.match.el);
      pp.chosen = mc.match.label;
      return { done: false, next: 'verify', waitMs: 250, method: lb ? 'role=option' : 'new-node text match', chosen: mc.match.label };
    }
    if (step === 'verify') {
      var pend = state.pending.get(ref) || {};
      var chosen = pend.chosen || opts.chosen;
      if (kind === 'combobox') {
        // Let the dropdown finish closing (MUI animates it out) so it doesn't cover the next field.
        if (!pend.verifyStart) pend.verifyStart = Date.now();
        if (findListbox(el) && Date.now() - pend.verifyStart < COVER_RETRY_MS) {
          state.pending.set(ref, pend);
          return { done: false, next: 'verify', waitMs: 150 };
        }
      }
      if (kind === 'contenteditable' && !pend.secondLook) {
        // An editor may reconcile and revert the DOM a moment later: look twice.
        pend.secondLook = true;
        state.pending.set(ref, pend);
        return { done: false, next: 'verify', waitMs: 300 };
      }
      stopPending(ref);
      var ok = false, shown = '';
      var want2 = lower(chosen || value);
      if (kind === 'text' || kind === 'datetime') { shown = el.value; ok = looksSame(shown, value); }
      else if (kind === 'select') { shown = currentValue(el, kind); ok = chosen ? lower(shown) === lower(chosen) : lower(shown).indexOf(lower(value)) >= 0; }
      else if (kind === 'checkbox' || kind === 'switch') { ok = isChecked(el) === toBool(value); shown = isChecked(el) ? 'checked' : 'unchecked'; }
      else if (kind === 'radio-group') { var sel = info.members.filter(isChecked)[0]; shown = sel ? accName(sel) : ''; ok = !!sel && (lower(shown) === want2 || lower(shown).indexOf(lower(value)) >= 0); }
      else if (kind === 'combobox') {
        if (el.tagName === 'INPUT') {
          var iv = lower(el.value);
          var cv = lower(containerValue(el, f.label));
          shown = el.value || containerValue(el, f.label);
          ok = (!!iv && (iv === want2 || looksSame(iv, want2))) || (!iv && (cv === want2 || cv.indexOf(want2) === 0));
        } else {
          var disp = lower(comboDisplay(el));
          shown = comboDisplay(el);
          if (disp) ok = disp === want2 || looksSame(disp, want2) || disp.indexOf(want2) === 0;
          else { var cv2 = lower(containerValue(el, f.label)); shown = cv2; ok = cv2 === want2 || cv2.indexOf(want2) === 0; }
        }
        if (!ok) dismiss(el);
      }
      else if (kind === 'contenteditable') { shown = norm(el.innerText); ok = looksSame(shown, value); }
      if ((kind === 'text' || kind === 'datetime' || kind === 'contenteditable') && isFocused(el)) el.blur();
      var invalid = el.getAttribute && el.getAttribute('aria-invalid') === 'true';
      var userInvalid = false;
      try { userInvalid = el.matches && el.matches(':user-invalid'); } catch (e) { /* ignore */ }
      var masked = !!f.sensitive || !!valueSensitivity(value);
      // The page may hide a field as soon as it holds a value. Undoing the write would break real
      // pages (a step that advances by itself), so the person is told instead. Being covered
      // does not count here: validation bubbles and tooltips cover fields for a moment.
      var hiddenAfter = null;
      if (ok) {
        var va = visibilityOf(visualTarget(el, kind), kind, el);
        if (!va.visible && va.reason !== 'covered') hiddenAfter = va.reason;
      }
      return {
        done: true, outcome: !ok ? 'mismatch' : hiddenAfter ? 'filled-then-hidden' : 'filled',
        detail: hiddenAfter ? 'the page hid this field after it was filled (' + hiddenAfter + ')' : undefined,
        readBack: masked ? '•••' : clip(shown, 120),
        validation: (invalid || userInvalid) ? (clip(el.validationMessage || describedBy(el), 160) || 'invalid') : null
      };
    }
    return { done: true, outcome: 'failed', detail: 'unknown step ' + step };
  }

  // ---------------------------------------------------------------- lifecycle
  // If the agent stops calling while a window is open (it crashed, lost its connection, or gave
  // up on this page while the page was stuck), the page goes back to normal by itself.
  function checkAgent() {
    if (!state.active) { clearInterval(state.watchdog); state.watchdog = 0; return; }
    if (Date.now() - state.lastCallAt < AGENT_GONE_MS) return;
    if (state.mode === 'fill') {
      end({ message: 'AI Expedite stopped. Nothing more will be filled. Check the form before you submit it.', lingerMs: 8000 });
      return;
    }
    state.approved = null; state.allowNavHref = null;
    closeGuard(0);
  }
  function begin(opts) {
    ensureUi();
    state.stopRequested = false; state.stopAt = 0; state.stopTrusted = null;
    state.approved = null; state.allowNavHref = null;
    openGuard('fill');
    say((opts && opts.label) || 'AI Expedite is filling this form');
    return { ok: true, docId: DOC_ID };
  }
  // The guards stay armed for a grace period, because a page can submit on a timer started by
  // the last change. Something the person does in that time (a click or a key) is let through.
  function end(opts) {
    closeGuard(GRACE_MS);
    clearHighlight();
    state.pending.forEach(function (p, ref) { stopPending(ref); });
    var summary = { ok: true, blockedSubmits: state.blockedSubmits.slice(), requestsDuringFill: state.requests.slice(),
      stopRequested: state.stopRequested, graceMs: GRACE_MS };
    var host = state.host;
    var finalMsg = opts && opts.message;
    if (host && finalMsg && state.msg) {
      state.msg.textContent = finalMsg;
      setTimeout(function () { if (host.isConnected) host.remove(); }, (opts && opts.lingerMs) || 4000);
    } else if (host) host.remove();
    state.host = null; state.outline = null; state.msg = null;
    return summary;
  }
  function status() {
    return {
      docId: DOC_ID, url: location.href, title: document.title, stopRequested: state.stopRequested, stopAt: state.stopAt,
      stopTrusted: state.stopTrusted, blockedSubmits: state.blockedSubmits.slice(), allowedByPerson: state.allowedByPerson.slice(),
      allowedByAgent: state.allowedByAgent.slice(), requests: state.requests.slice(), active: state.active, guarding: guarding(),
      hasFocus: document.hasFocus(), visibility: document.visibilityState, readyState: document.readyState
    };
  }
  function fingerprint(ref) {
    var info = fieldFor(ref);
    if (!info) return { exists: false };
    return { exists: true, fp: info.f.fp, visible: info.f.visible, hiddenReason: info.f.hiddenReason, sensitive: info.f.sensitive };
  }
  // Right before trusted typing: the field must still be visible and hold the focus.
  function check(ref) {
    var info = fieldFor(ref);
    if (!info) return { ok: false, why: 'not-found' };
    var why = recheck(info.el, info.kind);
    if (why) return { ok: false, why: why };
    if (!isFocused(info.el) && !info.el.contains(document.activeElement)) return { ok: false, why: 'lost focus' };
    return { ok: true };
  }
  function read(ref) {
    var info = fieldFor(ref);
    if (!info) return { exists: false };
    var f = info.f;
    return { exists: true, value: f.value, checked: f.checked, hasValue: f.hasValue };
  }

  // ---------------------------------------------------------------- reading a page
  // Would a click on t submit or save something? A real link (a GET to another document) is
  // not: it sends no form data. Everything the POC treats as submit-class otherwise is.
  function isSubmitClass(t) {
    if (!t || t.nodeType !== 1) return false;
    if (looksLikeSubmit(t) && !navigatingLink(t)) return true;
    var lab = t.closest && t.closest('label');
    if (lab && lab.control && lab.control !== t) {
      var c = lab.control;
      if (c.tagName === 'BUTTON' || (c.tagName === 'INPUT' && /^(submit|image|reset|button)$/i.test(c.type))) return true;
    }
    var act = t.closest && t.closest('button, input[type=submit], input[type=image], [role=button]');
    return !!(act && act !== t && (looksLikeSubmit(act) || (act.tagName === 'BUTTON' && act.type === 'submit' && act.form)));
  }
  function isButtonish(el) {
    if (!el || el.nodeType !== 1) return false;
    if (el.tagName === 'BUTTON') return true;
    if (el.tagName === 'INPUT' && /^(submit|button|image|reset)$/i.test(el.type)) return true;
    return lower(el.getAttribute('role')) === 'button';
  }
  function controlText(el) {
    return clip(norm(el.getAttribute('aria-label')) || norm(el.innerText) || norm(el.value) || norm(el.getAttribute('title')) || norm(el.getAttribute('alt')), 80);
  }
  function httpHref(a) {
    try { var u = new URL(a.href, location.href); return /^https?:$/.test(u.protocol) ? u.href : null; } catch (e) { return null; }
  }
  // The visible submit-class controls of a form (inside it, or tied to it with form=).
  function formSubmitControls(form) {
    var seen = new Set(), out = [];
    var cands = Array.prototype.slice.call(form.elements || []).concat(Array.prototype.slice.call(form.querySelectorAll('[role=button], a[href]')));
    for (var i = 0; i < cands.length && out.length < 10; i++) {
      var c = cands[i];
      if (seen.has(c) || inOwnUi(c)) continue;
      seen.add(c);
      if ((isButtonish(c) || c.tagName === 'A') && isShown(c) && isSubmitClass(c)) out.push(c);
    }
    return out;
  }
  function formName(form) {
    var n = norm(form.getAttribute('aria-label')) || idrefText(form, 'aria-labelledby') || norm(form.getAttribute('name')) || norm(form.id);
    if (!n) { var h = form.querySelector('h1,h2,h3,h4,legend'); if (h) n = norm(h.innerText); }
    if (!n) { var t = nearbyText(form); if (t) n = t; }
    return clip(n || document.title, 80);
  }
  function snapshot(opts) {
    opts = opts || {};
    var maxItems = opts.maxItems > 0 ? opts.maxItems : 300;
    var maxText = opts.maxText > 0 ? opts.maxText : 20000;
    var sc = scan();
    var text = '', textTruncated = false;
    if (opts.includeText !== false && document.body) {
      text = String(document.body.innerText || '').replace(/[ \t ]+\n/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
      if (text.length > maxText) { text = text.slice(0, maxText); textTruncated = true; }
    }
    var links = [], buttons = [], seenHref = {}, more = { links: false, buttons: false };
    for (var el of allElements(document)) {
      if (inOwnUi(el)) continue;
      if (el.tagName === 'A' && el.hasAttribute('href')) {
        if (links.length >= maxItems) { more.links = true; continue; }
        var href = httpHref(el);
        if (!href || !isShown(el)) continue;
        var lt = controlText(el);
        var key = href + '|' + lt;
        if (seenHref[key]) continue;
        seenHref[key] = 1;
        links.push({ ref: refFor(el), text: lt, href: href.length > 2000 ? href.slice(0, 2000) : href });
        continue;
      }
      if (isButtonish(el)) {
        if (buttons.length >= maxItems) { more.buttons = true; continue; }
        if (!isShown(el)) continue;
        buttons.push({ ref: refFor(el), text: controlText(el), submitClass: isSubmitClass(el) });
      }
    }
    var forms = [];
    Array.prototype.forEach.call(document.forms, function (form) {
      if (forms.length >= 50 || inOwnUi(form)) return;
      var controls = formSubmitControls(form);
      var hasField = sc.fields.some(function (f) { return f.formRef === refFor(form) && f.visible; });
      if (!controls.length && !hasField) return;
      forms.push({ formRef: refFor(form), name: formName(form),
        submitControls: controls.map(function (c) { return { ref: refFor(c), text: controlText(c) }; }) });
    });
    return {
      docId: DOC_ID, url: location.href, origin: location.origin, title: document.title,
      text: text, textTruncated: textTruncated, links: links, buttons: buttons, fields: sc.fields.slice(0, maxItems),
      fieldsTruncated: sc.fields.length > maxItems, linksTruncated: more.links, buttonsTruncated: more.buttons, forms: forms
    };
  }

  // ---------------------------------------------------------------- clicking (never a submit)
  // Where to click so the press lands on el itself: its centre, after scrolling it into view.
  // null (with why) when it cannot be seen or something else is on top of it.
  function clickTarget(el) {
    // A fill's closing message lingers over the page for a few seconds; the agent acting again
    // ends it, or it would cover (and take the press meant for) a control beneath it.
    Array.prototype.forEach.call(document.querySelectorAll('#' + HOST_ID), function (n) { if (n !== state.host) n.remove(); });
    el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
    var vis = visibilityOf(el, 'button', el);
    if (!vis.visible) return { why: vis.reason || 'hidden' };
    if (!vis.inViewport) return { why: 'off-screen' };
    var r = el.getBoundingClientRect();
    var x = r.left + r.width / 2, y = r.top + r.height / 2;
    // A control inside an open shadow root: the document only sees its outermost host, so the
    // host chain must be on top there, and the control itself on top inside its own root.
    var top = document.elementFromPoint(x, y);
    var rn = el.getRootNode ? el.getRootNode() : document;
    if (top && rn !== document && rn.elementFromPoint) {
      if (!composedContains(top, el)) return { why: 'covered' };
      top = rn.elementFromPoint(x, y);
    }
    if (!top || !(top === el || el.contains(top))) return { why: 'covered' };
    return { x: x, y: y };
  }
  // Is b inside a, crossing shadow boundaries (a shadow root's parent is its host)?
  function composedContains(a, b) {
    for (var n = b; n; n = n.parentNode || n.host) { if (n === a) return true; }
    return false;
  }
  // The agent is about to click a link or a control that does not submit: guard the page, so
  // the only thing that may leave it is the link's own navigation.
  function clickBegin(ref) {
    var el = byRef(ref);
    if (!el || inOwnUi(el)) return { ok: false, reason: 'not-found' };
    if (isSubmitClass(el)) return { ok: false, reason: 'submit-class', text: controlText(el) };
    var t = clickTarget(el);
    if (t.why) return { ok: false, reason: 'hidden', detail: t.why };
    var link = navigatingLink(el);
    state.approved = null;
    openGuard('click');
    state.allowNavHref = link ? link.href : null;
    state.agentActingUntil = Date.now() + AGENT_ACT_MS;
    return { ok: true, x: t.x, y: t.y, docId: DOC_ID, href: link ? httpHref(link) : null };
  }
  // AppleScript route: the click itself, from the page (no trusted input there).
  function clickNow(ref) {
    var el = byRef(ref);
    if (!el || state.mode !== 'click' || !state.active) return { ok: false, reason: 'not-found' };
    state.agentActingUntil = Date.now() + AGENT_ACT_MS;
    if (typeof el.click === 'function') el.click(); else pointerSequence(el);
    return { ok: true };
  }
  function clickEnd() {
    if (state.mode === 'click') { state.allowNavHref = null; closeGuard(1000); }
    return { ok: true, blockedSubmits: state.blockedSubmits.slice(), docId: DOC_ID };
  }

  // ---------------------------------------------------------------- preparing and submitting
  // The control and the form a submit ref names. A form ref picks its one visible submit
  // control; no ref picks the page's one form with one submit control. A control outside any
  // <form> (a script-driven page) submits what its nearest container holds.
  function virtualContainer(control) {
    var node = control.parentElement;
    for (var i = 0; i < 10 && node && node !== document.body; i++, node = node.parentElement) {
      if (node.querySelector(FIELD_SELECTOR)) return node;
    }
    return document.body || document.documentElement;
  }
  function resolveSubmit(ref) {
    var el = null;
    if (ref) {
      el = byRef(ref);
      if (!el || inOwnUi(el)) return { error: 'not-found' };
    }
    var form = null, control = null;
    if (el && el.tagName === 'FORM') {
      var cs = formSubmitControls(el);
      if (cs.length !== 1) return { error: cs.length ? 'ambiguous-submit' : 'no-submit', candidates: cs.map(function (c) { return { ref: refFor(c), text: controlText(c) }; }) };
      form = el; control = cs[0];
    } else if (el) {
      if (!isSubmitClass(el)) return { error: 'not-submit-control' };
      control = el;
      form = (control.form && control.form.tagName === 'FORM') ? control.form : control.closest('form');
    } else {
      var found = [];
      Array.prototype.forEach.call(document.forms, function (f) {
        if (inOwnUi(f)) return;
        var c = formSubmitControls(f);
        if (c.length) found.push({ form: f, controls: c });
      });
      if (found.length === 1 && found[0].controls.length === 1) { form = found[0].form; control = found[0].controls[0]; }
      else return { error: found.length ? 'ambiguous-form' : 'no-form', candidates: found.slice(0, 10).map(function (x) { return { ref: refFor(x.form), text: formName(x.form) }; }) };
    }
    return { control: control, form: form, container: form || virtualContainer(control) };
  }
  function rawValue(info) {
    if (info.kind === 'checkbox' || info.kind === 'switch') return isChecked(info.el) ? 'true' : 'false';
    if (info.kind === 'radio-group') { var sel = info.members.filter(isChecked)[0]; return sel ? (accName(sel) || sel.value || 'on') : ''; }
    var v = currentValue(info.el, info.kind);
    return v == null ? '' : String(v);
  }
  function inContainer(info, r) {
    if (r.form) return info.f.formRef === refFor(r.form);
    var el = info.kind === 'radio-group' ? info.members[0] : info.el;
    return !!el && r.container.contains(el);
  }
  // The absolute URL a form (or a submitter's formaction) posts to; an empty or missing action
  // posts to the document's own URL.
  function submitUrl(action) {
    try { return new URL(action || location.href, document.baseURI).href.split('#')[0]; } catch (e) { return String(action || ''); }
  }
  // Every name/value pair the submission would carry, in tree order, whether or not the person
  // can see or fill the control: hidden inputs (an amount, a recipient id), read-only and
  // unlabelled ones included. The fillable-field scan skips hidden inputs, so it cannot be the
  // only thing the digest covers. A sensitive value (a password) contributes only its length.
  // The controls come from the form's own `elements` getter (a control named "elements" shadows
  // the property) or, for a control outside any form, from its container.
  var FORM_ELEMENTS = Object.getOwnPropertyDescriptor(HTMLFormElement.prototype, 'elements').get;
  function submittedData(r, sensitiveEls) {
    var els = r.form ? Array.prototype.slice.call(FORM_ELEMENTS.call(r.form))
      : Array.prototype.slice.call(r.container.querySelectorAll('input, select, textarea'));
    var out = [];
    els.forEach(function (el) {
      if (inOwnUi(el)) return;
      var tag = el.tagName, type = lower(el.getAttribute('type') || '');
      if (tag !== 'INPUT' && tag !== 'SELECT' && tag !== 'TEXTAREA') return;   // buttons: the submitter is in parts.submit
      if (tag === 'INPUT' && /^(submit|image|reset|button)$/.test(type)) return;
      var name = el.getAttribute('name') || '';
      if (!name) return;
      var disabled = false;
      try { disabled = el.matches(':disabled'); } catch (e) { disabled = !!el.disabled; }
      if (disabled) return;
      var push = function (v) {
        v = v == null ? '' : String(v);
        var sens = sensitiveEls.has(el) || type === 'password';
        out.push({ name: name, value: sens ? '' : v, len: sens ? v.length : 0, sensitive: sens });
      };
      if (tag === 'INPUT' && (type === 'checkbox' || type === 'radio')) { if (el.checked) push(el.value); return; }
      if (tag === 'INPUT' && type === 'file') {
        Array.prototype.forEach.call(el.files || [], function (f) { push(f.name + '␟' + f.size); });
        return;
      }
      if (tag === 'SELECT') {
        Array.prototype.forEach.call(el.options, function (o) { if (o.selected && !o.disabled) push(o.value); });
        return;
      }
      push(el.value);
    });
    return out;
  }
  // What the digest covers, in DOM order: the page, the form's identity, every field's identity
  // and current value, and the submit control. A sensitive value (a password the person typed)
  // contributes only its length, so its hash never leaves the computer.
  function submitParts(r) {
    var sc = scan();
    var parts = { origin: location.origin, path: location.pathname, form: '', fields: [], data: [], submit: '', kind: r.form ? 'form' : 'virtual' };
    var summary = [];
    var c = r.control;
    var formEl = r.form || r.container;
    // Where the data goes is part of what the person approved: the form's resolved action,
    // method and encoding, and the submitter's own formaction / formmethod / formenctype /
    // formtarget overrides (any activation of the control honours them). Attributes,
    // never the form's IDL properties, which a field named "action" or "method" shadows. The
    // form's target is left out: the guard itself retargets forms to _self while it runs.
    parts.form = fnv([formEl.tagName, r.form ? 'form' : 'virtual', formEl.id || '', formEl.getAttribute('name') || '',
      r.form ? (r.form.getAttribute('action') || '') : '', r.form ? lower(r.form.getAttribute('method') || 'get') : '',
      r.form ? submitUrl(r.form.getAttribute('action')) : '', r.form ? lower(r.form.getAttribute('enctype') || '') : ''].join('␟'));
    parts.submit = fnv([c.tagName, lower(c.type || ''), c.getAttribute('name') || '', c.getAttribute('value') || '', c.id || '', controlText(c),
      c.hasAttribute('formaction') ? submitUrl(c.getAttribute('formaction')) : '', lower(c.getAttribute('formmethod') || ''),
      lower(c.getAttribute('formenctype') || ''), c.getAttribute('formtarget') || ''].join('␟'));
    var sensitiveEls = new Set();
    sc.fields.forEach(function (f) {
      var info = fieldFor(f.ref);
      if (!info || !inContainer(info, r)) return;
      var raw = rawValue(info);
      var sensitive = !!f.sensitive;
      if (sensitive) (info.members || [info.el]).forEach(function (m) { if (m) sensitiveEls.add(m); });
      parts.fields.push({ fp: f.fp, value: sensitive ? '' : raw, len: sensitive ? raw.length : 0, sensitive: sensitive });
      var shown = (f.kind === 'checkbox' || f.kind === 'switch') ? (raw === 'true' ? 'checked' : 'unchecked') : (sensitive ? (raw ? '•••' : '') : clip(raw, 500));
      if (!f.visible && !norm(raw)) return;    // an empty field nobody can see sends nothing worth showing
      var row = { label: clip(f.visibleLabel || f.label || f.name || f.kind, 120), kind: f.kind, value: shown, required: !!f.required };
      if (!f.visible) row.hidden = true;
      if (sensitive) row.sensitive = true;
      if (summary.length < 300) summary.push(row);
    });
    parts.data = submittedData(r, sensitiveEls);
    return { parts: parts, summary: summary };
  }
  function prepareSubmit(ref) {
    var r = resolveSubmit(ref);
    if (r.error) return { ok: false, reason: r.error, candidates: r.candidates || [] };
    var t = clickTarget(r.control);
    if (t.why) return { ok: false, reason: 'hidden', detail: t.why };
    var sp = submitParts(r);
    return {
      ok: true, docId: DOC_ID, url: location.href, origin: location.origin, title: document.title,
      submitRef: refFor(r.control), submitText: controlText(r.control), formName: r.form ? formName(r.form) : clip(document.title, 80),
      parts: sp.parts, token: fnv(JSON.stringify(sp.parts)), fields: sp.summary
    };
  }
  // Re-reads what prepareSubmit read, for the approved control, so the agent can compare digests.
  function submitCheck(ref) {
    var c = byRef(ref);
    if (!c) return { ok: false, reason: 'not-found' };
    var r = resolveSubmit(ref);
    if (r.error) return { ok: false, reason: r.error };
    var sp = submitParts(r);
    return { ok: true, docId: DOC_ID, parts: sp.parts, token: fnv(JSON.stringify(sp.parts)) };
  }
  // The person approved this submit and the agent checked the digest: lift the guard for exactly
  // this control, once. token is the cheap hash submitCheck returned; anything that changed in
  // between refuses. With how = 'scriptClick' (AppleScript) the kit clicks the control right here;
  // otherwise it returns the point for the agent's trusted click.
  function armSubmit(ref, token, how) {
    var r = resolveSubmit(ref);
    if (r.error) return { ok: false, reason: r.error };
    if (fnv(JSON.stringify(submitParts(r).parts)) !== token) return { ok: false, reason: 'changed' };
    var t = clickTarget(r.control);
    if (t.why) return { ok: false, reason: 'hidden', detail: t.why };
    openGuard('submit');
    state.allowNavHref = null;
    // Where the approved form may post when a script submits it: its own action, or the
    // control's formaction (the digest just confirmed both are the approved ones).
    var actions = [];
    if (r.form) {
      actions.push(submitUrl(r.form.getAttribute('action')));
      if (r.control.hasAttribute('formaction')) actions.push(submitUrl(r.control.getAttribute('formaction')));
    }
    state.approved = { el: new WeakRef(r.control), form: r.form ? new WeakRef(r.form) : null, virtual: !r.form,
      until: Date.now() + APPROVED_MS, used: false, navUsed: false, pressed: false, actions: actions };
    state.agentActingUntil = Date.now() + AGENT_ACT_MS;
    if (how === 'scriptClick') {
      // Activate the control as a press would: its own click handlers run (a page that saves by
      // fetch in a submit button's onclick and cancels the native submission), and a native
      // submit button then submits its form with itself as the submitter, validation included.
      // requestSubmit(c) would skip those handlers and post the form natively instead.
      r.control.click();
      return { ok: true, docId: DOC_ID, submitted: true };
    }
    return { ok: true, docId: DOC_ID, x: t.x, y: t.y };
  }
  // used: the approved submission fired its submit event. invalid: the browser's validation
  // refused it. requests: fetch / XHR the page made meanwhile (a script-driven save sends one).
  function submitEnd() {
    var used = !!(state.approved && state.approved.used);
    var invalid = state.mode === 'submit' && !!state.sawInvalid;
    var requests = state.requests.length;
    if (state.mode === 'submit') { state.approved = null; closeGuard(0); }
    return { ok: true, docId: DOC_ID, used: used, invalid: invalid, requests: requests, blockedSubmits: state.blockedSubmits.slice() };
  }

  var API = {
    ping: function () { return { version: VERSION, docId: DOC_ID, url: location.href, title: document.title, readyState: document.readyState, hasFocus: document.hasFocus(), visibility: document.visibilityState }; },
    scan: scan, fingerprint: fingerprint, check: check, read: read, begin: begin, end: end, status: status, fillStep: fillStep,
    clearHighlight: function () { clearHighlight(); return { ok: true }; },
    snapshot: snapshot, clickBegin: clickBegin, clickNow: clickNow, clickEnd: clickEnd,
    prepareSubmit: prepareSubmit, submitCheck: submitCheck, armSubmit: armSubmit, submitEnd: submitEnd
  };

  globalThis.__aixKit = {
    version: VERSION,
    docId: DOC_ID,
    call: function (name, args) {
      state.lastCallAt = Date.now();
      try {
        var fn = API[name];
        if (!fn) throw new Error('unknown kit call: ' + name);
        var result = fn.apply(null, args || []);
        // Every reply carries the Stop flag and the document id, so the agent notices a click on
        // Stop or a navigation on the very next call, without an extra round trip.
        return JSON.stringify({ ok: true, result: result, stop: state.stopRequested, stopAt: state.stopAt, docId: DOC_ID });
      } catch (e) {
        return JSON.stringify({ ok: false, error: String((e && e.stack) || e), stop: state.stopRequested, docId: DOC_ID });
      }
    },
    _teardown: function () { try { end(); teardownGuards(); } catch (e) { /* ignore */ } }
  };
})();
