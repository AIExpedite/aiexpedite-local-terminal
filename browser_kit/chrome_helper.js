// AI Expedite browser control: the AppleScript (JXA) bridge to Google Chrome. macOS only.
//
// One long-lived osascript process: it reads one JSON request per line on stdin and writes one
// JSON reply per line on stdout, so each call costs one Apple Event, not a process spawn.
//
// It never launches Chrome (every request but "status" checks running() first) and only ever
// runs the kit script the agent passes in, in the tab the agent names. Tab ids are Chrome's own
// ids as text (Chrome 156 made them text; older versions use integers): a tab is found by
// searching the windows, so a tab dragged to another window keeps working.
ObjC.import('Foundation');

function writeLine(s) {
  $.NSFileHandle.fileHandleWithStandardOutput.writeData($(s + '\n').dataUsingEncoding($.NSUTF8StringEncoding));
}

function findTab(chrome, id) {
  var ws = chrome.windows();
  for (var i = 0; i < ws.length; i++) {
    var ids = ws[i].tabs.id();
    for (var j = 0; j < ids.length; j++) {
      if (String(ids[j]) === String(id)) return ws[i].tabs[j];
    }
  }
  throw new Error('tab-not-found');
}

function describeTab(t) {
  return { tabId: String(t.id()), title: t.title(), url: t.url(), loading: t.loading() };
}

function handle(chrome, req) {
  if (req.op === 'status') return { running: chrome.running() };
  if (!chrome.running()) throw new Error('chrome-not-running');
  if (req.op === 'list') {
    // One Apple Event per property per window, not per tab.
    var out = [];
    var ws = chrome.windows();
    for (var i = 0; i < ws.length; i++) {
      var win = ws[i];
      var activeId = String(win.activeTab.id());
      var ids = win.tabs.id(), titles = win.tabs.title(), urls = win.tabs.url();
      for (var j = 0; j < ids.length; j++) {
        out.push({ tabId: String(ids[j]), title: titles[j], url: urls[j], active: i === 0 && String(ids[j]) === activeId });
      }
    }
    return out;
  }
  if (req.op === 'open') {
    var wins = chrome.windows();
    var w = wins.length ? wins[0] : null;
    if (!w) {
      chrome.Window().make();
      w = chrome.windows[0];
    }
    var before = {};
    w.tabs.id().forEach(function (x) { before[String(x)] = 1; });
    w.tabs.push(chrome.Tab({ url: req.url }));
    var after = w.tabs.id();
    for (var k = after.length - 1; k >= 0; k--) {
      if (!before[String(after[k])]) return describeTab(w.tabs[k]);
    }
    return describeTab(w.activeTab);
  }
  var tab = findTab(chrome, req.tabId);
  if (req.op === 'info') return describeTab(tab);
  if (req.op === 'navigate') { tab.url = req.url; return { ok: true }; }
  if (req.op === 'back') { tab.goBack(); return { ok: true }; }
  if (req.op === 'forward') { tab.goForward(); return { ok: true }; }
  if (req.op === 'reload') { tab.reload(); return { ok: true }; }
  if (req.op === 'exec') {
    var r = tab.execute({ javascript: req.code });
    return (r === undefined || r === null) ? null : String(r);
  }
  throw new Error('unknown op ' + req.op);
}

function run(argv) {
  var chrome = Application('Google Chrome');
  var stdin = $.NSFileHandle.fileHandleWithStandardInput;
  var buf = '';
  writeLine(JSON.stringify({ id: 0, ok: true, result: 'ready' }));
  while (true) {
    var data = stdin.availableData;
    // NSData's length bridges as a string-like value ("0"), so compare as a
    // number: at EOF (the agent went away) the helper must exit, not spin.
    if (Number(data.length) === 0) break;
    buf += $.NSString.alloc.initWithDataEncoding(data, $.NSUTF8StringEncoding).js;
    var nl;
    while ((nl = buf.indexOf('\n')) >= 0) {
      var line = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      if (!line.trim()) continue;
      var req;
      try { req = JSON.parse(line); } catch (e) { writeLine(JSON.stringify({ id: -1, ok: false, error: 'bad request json' })); continue; }
      var t0 = Date.now();
      try {
        writeLine(JSON.stringify({ id: req.id, ok: true, result: handle(chrome, req), ms: Date.now() - t0 }));
      } catch (e) {
        writeLine(JSON.stringify({ id: req.id, ok: false, error: String((e && e.message) || e), code: (e && e.errorNumber) || 0, ms: Date.now() - t0 }));
      }
    }
  }
  return '';
}
