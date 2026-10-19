/* termcp Web UI — the extension panel.
 *
 * The right-hand column is the connections panel's other side. NetHub lists the
 * machines Termcp can dial; this lists the trimmed sessions Termcp keeps warm
 * for whoever is driving it (an agent over MCP, mostly). A row is a template's
 * runtime verdict — up, trying, down with a reason — plus the one handle an up
 * extension is driven through.
 *
 * The panel owns no state of its own. The socket delivers the whole list on
 * every change, because the reconciler broadcasts after each state transition,
 * so renderExtensionGrid is a full repaint with nothing to reconcile against.
 * An empty list keeps the column: it is where a new extension is created from,
 * and a panel that vanishes when it is empty is a panel the author cannot open
 * to author with.
 *
 * Its chrome is the connections panel's, twice over: the same batch row on top
 * (select, invert, open, export, delete) and the same pair of bottom keys
 * (author one, import a set). Where NetHub's controls dial and delete hosts,
 * these open the terminal an extension is served by and delete templates.
 *
 * Authoring goes through the same store the extension MCP tool writes to — the
 * REST routes are that store's other caller — so a template saved here and one
 * saved by an agent are indistinguishable, and the two surfaces cannot drift
 * apart on what a valid template is. The dialog has two pages: Status is what a
 * card click opens (the verdict, the handles, the failure history, and the
 * actions that follow from them), Edit is the template itself, as fields or as
 * the TOML file the store holds.
 *
 * Loaded as a plain synchronous <script> like every other module: the
 * declarations land in the shared global scope, and the socket's frame handler
 * is what calls into it.
 */

// The last frame delivered, kept only so a language switch can repaint the
// cards from the same rows rather than from a cache of its own.
var _extRows = null;

// Reconciler statuses, in the order the panel lists them: what can be used
// first, what is being brought up, what needs attention, what was put away.
var EXT_STATUS_ORDER = ['available', 'probing', 'unavailable', 'disabled'];

// The lamp class is keyed on the status, so a status the panel has never seen
// falls back to the down lamp rather than to no dot at all.
var EXT_LAMP_CLASSES = {
  available: 'is-online',
  probing: 'is-connecting',
  unavailable: 'is-offline',
  disabled: 'is-disabled'
};

// A clockwise arrow, read as "try again": the one per-card key, for the
// repair an author is most often looking at a down extension to make.
// A trash can, read as "remove this extension": the card is where an author
// looks when a template is no longer wanted.
var SVG_EXT_RETRY = '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M14 8a6 6 0 11-1.9-4.4"/><path d="M14 1.6V4.6H11"/></svg>';

function extLampClass(status) {
  return EXT_LAMP_CLASSES[status] || 'is-offline';
}

/* The catalog keys are spelt out in the switches rather than concatenated
   (`t('ext.status.' + status)`) so a reader grepping for a string finds the
   panel, and so a status the panel has never seen falls back to the word for
   "unavailable" instead of a raw key on screen. */
function extStatusLabel(status) {
  switch (status) {
    case 'available': return t('ext.status.available');
    case 'probing': return t('ext.status.probing');
    case 'disabled': return t('ext.status.disabled');
    default: return t('ext.status.unavailable');
  }
}

/* The three presets the config ships; anything else is a custom template and is
   shown as its own kind verbatim rather than mislabelled. */
function extKindLabel(kind) {
  switch (kind) {
    case 'mcp': return t('ext.kind.mcp');
    case 'skill': return t('ext.kind.skill');
    case 'prompt': return t('ext.kind.prompt');
    default: return kind || '';
  }
}

// The forward modal already names the three directions; reusing those strings
// keeps one vocabulary for one concept, and an unknown kind still has a label.
function extForwardLabel(kind) {
  var key = 'modal.forward.dir.' + kind;
  var s = t(key);
  return (s === key) ? (kind || '') : s;
}

/* Sort by what the reader is here for, then by name: an extension that is up is
   above one that is trying, which is above one that is down, and a disabled one
   is out of the way at the bottom. */
function extRowOrder(a, b) {
  var ai = EXT_STATUS_ORDER.indexOf(a.status);
  var bi = EXT_STATUS_ORDER.indexOf(b.status);
  if (ai < 0) ai = EXT_STATUS_ORDER.length;
  if (bi < 0) bi = EXT_STATUS_ORDER.length;
  if (ai !== bi) return ai - bi;
  return String(a.name || '').localeCompare(String(b.name || ''));
}

/** Every extension the panel lists, in the order it lists them: the set the
 *  batch keys act on, read from the last frame rather than from the DOM. */
function selectableExtensionNames() {
  return (_extRows || []).slice().sort(extRowOrder).map(function (e) { return e.name; })
    .filter(function (n) { return !!n; });
}

function extRowByName(name) {
  var rows = _extRows || [];
  for (var i = 0; i < rows.length; i++) {
    if (rows[i] && rows[i].name === name) return rows[i];
  }
  return null;
}

function renderExtensionGrid(rows) {
  _extRows = rows || [];
  var panel = document.getElementById('extpanel');
  var grid = document.getElementById('ext-grid');
  var rail = document.getElementById('extpanel-rail');
  var frame = document.querySelector('.workspace-frame');
  if (!panel || !grid || !frame) return;
  var list = _extRows.slice().sort(extRowOrder);
  /* The column is permanent in the markup (.ext-on is on the frame from the
     start), so an empty list is laid out exactly like a full one and the empty
     state is where the first extension is created from — a panel that
     disappeared when empty was a panel the author could not open to author
     with. */
  grid.innerHTML = '';
  if (list.length === 0) {
    grid.appendChild(renderExtensionEmpty());
  } else {
    list.forEach(function (ext) {
      grid.appendChild(renderExtensionCard(ext));
    });
  }
  /* The rail is the collapsed column's whole content, so it is repainted from
     the same frame and keeps the same order as the cards beside it. */
  if (rail) {
    rail.innerHTML = '';
    list.forEach(function (ext) {
      rail.appendChild(renderExtensionRailItem(ext));
    });
  }
  /* Selection is derived from the set, never read back from the DOM, and it is
     pruned against what is still listed: a template that disappeared while the
     batch was being picked must not stay ticked, or the count lies and a delete
     is sent for something already gone. The prune rides on rows having arrived —
     an empty list would drop a tick made before the first frame. */
  _extBatch.syncCards(!!_extRows && _extRows.length > 0);
  _extBatch.update();
  /* A repaint preserves the state the page is in rather than re-deriving it:
     the stored preference says what the desktop sidebar should be, and reading
     it here would reopen a drawer that arrived closed below the breakpoint. */
  setExtensionsCollapsed(extensionsCollapsed(), false);
  /* The cards are live: a frame that arrives while one is open is what it was
     opened to watch, so every open card repaints from the row that just changed. */
  extRepaintWindows();
}

/* The empty state is the panel's reason to exist when nothing is configured: it
   says what an extension is, so an empty column is not a blank track. It carries
   no key of its own — the column closes with New and Import, and a third
   "create one" in the middle would be the same action twice. A socket frame that
   arrives before the first paint renders it too. */
function renderExtensionEmpty() {
  var row = document.createElement('div');
  row.className = 'ext-empty';
  row.innerHTML = '<p class="ext-empty-text">' + escapeHtml(t('ext.empty.text')) + '</p>';
  return row;
}

function renderExtensionCard(ext) {
  var tile = document.createElement('div');
  var lamp = extLampClass(ext.status);
  var retryable = ext.status === 'unavailable' || ext.status === 'probing';
  var selected = _extBatch.has(ext.name);
  tile.className = 'conn-tile entry-card ext-card' + (selected ? ' batch-selected' : '');
  tile.setAttribute('data-ext-name', ext.name || '');
  tile.setAttribute('data-ext-status', ext.status || '');
  var name = ext.name || '';
  var retryBtn = retryable
    ? '<button type="button" class="ext-retry-btn" title="' + escapeHtml(t('ext.retry.title')) +
      '" aria-label="' + escapeHtml(t('ext.retry.title')) + '">' + SVG_EXT_RETRY + '</button>'
    : '';
  /* The gear opens the configuration, the way the host cards' own gear does: the
     card is the readout, and the one thing a human reaches for by hand is filed
     behind the key that means "settings". */
  var gearBtn = '<button type="button" class="conn-edit-btn" title="' + escapeHtml(t('conn.aria.editProfile')) +
    '" aria-label="' + escapeHtml(t('conn.aria.editProfile')) + '">' + SVG_CONN_EDIT + '</button>';
  var meta =
    '<span class="ext-kind">' + escapeHtml(extKindLabel(ext.kind)) + '</span>' +
    '<span class="node-sep" aria-hidden="true">·</span>' +
    '<span class="node-status ' + lamp + '">' + escapeHtml(extStatusLabel(ext.status)) + '</span>';
  var target = ext.target ? '<div class="node-addr">' + escapeHtml(ext.target) + '</div>' : '';
  var fw = '';
  (ext.forwards || []).forEach(function (f) {
    if (!f.listen_addr) return;
    fw += '<div class="ext-fwd" title="' + escapeHtml(t('ext.fwd.title', { kind: extForwardLabel(f.kind) })) + '">' +
      escapeHtml(f.listen_addr) + '</div>';
  });
  // One line, not the history: the verdict is what the panel is for, and the
  // card's own window serves the rest.
  var fail = '';
  if (ext.failure && ext.failure.reason) {
    fail = '<div class="ext-fail" title="' + escapeHtml(t('ext.fail.title', { stage: ext.failure.stage || '' })) + '">' +
      escapeHtml(ext.failure.reason) + '</div>';
  }
  tile.innerHTML =
    '<div class="entry-card-inner">' +
    '<span class="node-lamp ' + lamp + '" aria-hidden="true"></span>' +
    '<div class="entry-card-main">' +
    '<div class="conn-nm-row">' +
    '<span class="conn-nm-cluster">' +
    '<span class="conn-nm">' + escapeHtml(name) + '</span>' +
    '</span>' +
    retryBtn +
    gearBtn +
    '</div>' +
    '<div class="node-meta">' + meta + '</div>' +
    target +
    fw +
    fail +
    '</div>' +
    '</div>';
  var inner = tile.querySelector('.entry-card-inner');
  if (inner) {
    inner.title = _extBatch.isOn() ? t('nethub.batch.selectOne', { name: name }) : t('ext.tip.status');
    inner.addEventListener('click', function (e) {
      if (e.target.closest('.ext-retry-btn, .conn-edit-btn')) return;
      /* In select mode a click ticks the card, the way the host cards behave:
         clicking through to a dialog while the row is being picked for a batch
         would lose the batch. */
      if (_extBatch.isOn()) { _extBatch.toggleSelection(name, tile); return; }
      /* A status card is a readout, not a terminal the click is aiming at: it opens
         centred, in the cascade, wherever the pointer happened to be. */
      openExtensionWindow(name, null);
    });
  }
  var btn = tile.querySelector('.ext-retry-btn');
  if (btn) {
    btn.addEventListener('click', function (e) {
      e.stopPropagation();
      retryExtension(name, btn);
    });
  }
  var gear = tile.querySelector('.conn-edit-btn');
  if (gear) {
    gear.addEventListener('click', function (e) {
      e.stopPropagation();
      openExtensionDialog(name);
    });
  }
  return tile;
}

/* Ask the reconciler to try again now. The human's nudge is the same call the
   agent's extension(action=retry) makes, so the two cannot drift apart in what
   they are allowed to do. The card stays pressed until the next frame lands;
   the repaint is the receipt. */
function retryExtension(name, btn) {
  if (!name) return;
  if (btn) btn.disabled = true;
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name) + '/retry', { method: 'POST' })
    .then(function (r) {
      if (!r.ok) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          throw new Error(j.error || r.status);
        });
      }
    })
    .catch(function (err) {
      if (btn) btn.disabled = false;
      showCopyToast(t('ext.toast.retryFailed', { msg: String((err && err.message) || err) }));
    });
}

/* The collapsed column's row: a lamp and a name, and nothing else. The status
   is the whole point — the rail exists so "which extensions are up" still has
   an answer when the cards are put away — and the name is what the tooltip
   holds, because a lamp alone will not say which machine it is. */
function renderExtensionRailItem(ext) {
  var btn = document.createElement('button');
  var lamp = extLampClass(ext.status);
  var name = ext.name || '';
  btn.type = 'button';
  btn.className = 'extpanel-rail-item';
  btn.title = name + ' — ' + extStatusLabel(ext.status);
  btn.setAttribute('aria-label', name + ', ' + extStatusLabel(ext.status));
  btn.innerHTML = '<span class="extpanel-rail-lamp ' + lamp + '" aria-hidden="true"></span>';
  btn.addEventListener('click', function (e) {
    e.stopPropagation();
    setExtensionsCollapsed(false, true);
  });
  return btn;
}

/* ---- Batch selection ---
 * The extension column's instance of the shared component (batch-bar.js). Its
 * verb is "open the cards": a card here is a status window, so the batch is
 * simply several of them, where a host batch dials one session per host. */
var _extBatch = createBatchBar({
  bodyId: 'extpanel-body',
  toggleId: 'ext-select-toggle',
  countId: 'ext-toggle-count',
  invertId: 'ext-sel-invert',
  splitId: 'ext-open-split',
  mainId: 'ext-batch-open',
  caretId: 'ext-open-caret',
  menuId: 'ext-open-menu',
  exportId: 'ext-menu-export',
  deleteId: 'ext-menu-delete',
  actionIds: ['ext-menu-export', 'ext-menu-delete'],
  gridId: 'ext-grid',
  cardSelector: '.conn-tile[data-ext-name]',
  nameAttr: 'data-ext-name',
  selectingClass: 'ext-selecting',
  selectKey: 'nethub.batch.select',
  names: selectableExtensionNames,
  render: function () { renderExtensionGrid(_extRows || []); },
  tipFor: function () { return t('ext.tip.status'); },
  onOpen: function (names) { openSelectedExtensions(names); },
  onExport: function (names) { exportSelectedExtensions(names); },
  onDelete: function (names) { deleteSelectedExtensions(names); }
});

/* Open the ticked extensions: one card per extension, the way a host batch opens
   one session per ticked host. A card is a window, so several coexist, drag and
   tile together, and the batch is simply several of them. */
function openSelectedExtensions(names) {
  if (!names || !names.length) return;
  if (extIsDrawer()) setExtensionsCollapsed(true, false);
  names.forEach(function (name) { openExtensionWindow(name, null); });
  // The ticks are spent: clear the selection, keep the mode on for the next batch.
  _extBatch.clear();
  _extBatch.repaint();
}

/* Delete the ticked templates. Deleting a template closes the connection kept
   warm for it once nothing else is served by that connection, which is what the
   dialog says: the author is removing both the recipe and what is running from
   it. */
function deleteSelectedExtensions(names) {
  if (!names || !names.length) return;
  var message = tCount('ext.batch.deleteMsg.one', 'ext.batch.deleteMsg.other', { count: names.length });
  confirmDialog({
    title: t('ext.batch.deleteTitle'),
    message: message,
    okText: t('batch.del.ok', { count: names.length }),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    /* One request for every name (comma-separated path, like the session and
       profile routes): the server reports per-name outcomes and a name that is
       already gone counts as cleared here. */
    deleteResourcesBatch('extensions', names).then(function (results) {
      var failed = [];
      (results || []).forEach(function (r) {
        if (r.ok || r.code === 'extension_not_found') {
          /* A removed template takes its own card with it, exactly as the single
             delete does: a window left behind says "this extension is gone" over
             a readout of something the author just removed. */
          _extBatch.deselect(r.id);
          var win = extWinFor(r.id);
          if (win) closeShellWindow(win);
        } else {
          failed.push(r.id + ': ' + (r.error || r.code || 'failed'));
        }
      });
      if (failed.length) showCopyToast(t('toast.delete.failed', { msg: failed.join('; ') }));
      if (_extDialogName && names.indexOf(_extDialogName) >= 0) closeExtensionDialog();
    }).catch(function (err) {
      showCopyToast(t('toast.delete.failed', { msg: String((err && err.message) || err) }));
    });
  });
}

/* The TOML download. The backend composes the document — the selection, or every
   stored template when the export-all key is used — so the browser stays a pipe
   for bytes. */
function downloadExtensionsToml(names) {
  var path = apiPath('/api/extensions/batch');
  if (names && names.length) path += '?names=' + names.map(encodeURIComponent).join(',');
  return downloadTOML(path, 'termcp-extensions.toml');
}

function exportSelectedExtensions(names) {
  if (!names || !names.length) return;
  downloadExtensionsToml(names).catch(function (e) {
    showCopyToast(t('conn.batch.exportFailed', { msg: String((e && e.message) || e) }));
  });
}

/* ---- Collapse state -------------------------------------------------------
 * The same three widths and one manual preference NetHub keeps, and for the
 * same reason: this is the connections panel's other side, so it answers the
 * same question the same way.
 *
 *   wide (>=1200px)   expanded by default
 *   medium (800-1199) collapsed (rail) by default
 *   narrow (<800px)   the panel is a drawer, and "collapsed" means closed
 *
 * The key is the panel's own; NetHub's preference does not decide this column,
 * and vice versa. Only a toggle above the drawer breakpoint writes it — below it
 * the panel is an overlay, and the overlay's open and close are the page's
 * business rather than the sidebar's preference. But the two drawers share one
 * scrim and one page, so opening one closes the other — two overlays at once is
 * two frosted panes over a page that cannot scroll behind either. */
var EXT_STATE_KEY = 'termcp.extensions';

function storedExtensionsCollapsed() {
  var v = null;
  try { v = localStorage.getItem(EXT_STATE_KEY); } catch (e) {}
  if (v === 'collapsed') return true;
  if (v === 'expanded') return false;
  return null;
}

/* Mirrors the stylesheet's breakpoint. Below it the body is fixed and slides,
   so the state means closed rather than railed — and the frame's collapsed
   class is kept off it, or the third grid track would still be 56px of rail on
   a page that no longer has grid columns at all. */
function extIsDrawer() {
  return (window.innerWidth || 0) < 800;
}

function extDefaultCollapsed() {
  return (window.innerWidth || 0) < 1200;
}

function setExtensionsCollapsed(collapsed, persist) {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('extpanel-body');
  var trigger = document.getElementById('extpanel-toggle');
  if (!frame || !body) return;
  collapsed = !!collapsed;
  var drawer = extIsDrawer();
  /* The two drawers share one scrim and one page, so the one being opened puts
     the other away. The call is guarded by "am I opening" rather than fired on
     every state write, and closing never calls across, so the pair cannot
     recurse into each other. */
  if (drawer && !collapsed && !netHubCollapsed()) setNetHubCollapsed(true, false);
  frame.classList.toggle('ext-collapsed', collapsed && !drawer);
  body.classList.toggle('drawer-open', drawer && !collapsed);
  /* The scrim and the page lock follow BOTH drawers (paintDrawerScrim in
     dialogs.js), so collapsing this panel no longer drops the overlay the
     connections drawer is still depending on. */
  if (typeof paintDrawerScrim === 'function') paintDrawerScrim();
  if (trigger) trigger.setAttribute('aria-expanded', String(!collapsed));
  /* The drawer scope never writes the preference: below the breakpoint the
     panel is an overlay, and dismissing an overlay is not a statement about how
     the desktop sidebar should sit. Writing it here would rail a column the
     wide screen later arrives at — the panel found collapsed for a decision
     made about a different shape of the same page. The overlay's state lives in
     the page, and the sidebar keeps its own. */
  if (persist && !drawer) {
    try { localStorage.setItem(EXT_STATE_KEY, collapsed ? 'collapsed' : 'expanded'); } catch (e) {}
  }
}

function extensionsCollapsed() {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('extpanel-body');
  if (extIsDrawer()) return !(body && body.classList.contains('drawer-open'));
  return !!(frame && frame.classList.contains('ext-collapsed'));
}

function currentExtensionsCollapsed() {
  var stored = storedExtensionsCollapsed();
  return stored === null ? extDefaultCollapsed() : stored;
}

/* A drawer is an overlay over the page, and an overlay does not open itself.
   "Expanded" was stored on a wide screen, where it means the sidebar is out;
   carried below the breakpoint it means a drawer over a page that cannot scroll
   behind it, with a scrim the user never asked for. The stored value still
   decides the desktop sidebar — this is the state the panel arrives in, not a
   rewrite of the preference. */
function initialExtensionsCollapsed() {
  return extIsDrawer() ? true : currentExtensionsCollapsed();
}

function toggleExtensions() {
  setExtensionsCollapsed(!extensionsCollapsed(), true);
}

/* The drawer's close key and the drawer-close button both reach for this, so
   the panel has one door from the inside as well as out. */
window.termcpToggleExtensions = function (open) {
  setExtensionsCollapsed(open === false, true);
};

/* A viewport change can move the page across a breakpoint in either direction,
   and the stored preference is re-applied, never rewritten — a resize is not a
   decision. Only a crossing is acted on: a resize that stays on one side of the
   breakpoint changes the panel's geometry but not its state, and re-applying
   would close a drawer the user is in the middle of using. Crossing into the
   drawer scope starts closed, so an overlay never opens itself; crossing back
   out keeps a drawer the user had open as the expanded column it becomes,
   rather than re-deriving it from storage — a rotation into a width whose
   default is the rail would otherwise put away a panel the user was reading. */
var _extWasDrawer = extIsDrawer();
var _extResizeTimer = 0;
window.addEventListener('resize', function () {
  if (_extResizeTimer) clearTimeout(_extResizeTimer);
  _extResizeTimer = setTimeout(function () {
    _extResizeTimer = 0;
    var drawer = extIsDrawer();
    if (drawer === _extWasDrawer) return;
    var wasDrawer = _extWasDrawer;
    _extWasDrawer = drawer;
    if (drawer) {
      setExtensionsCollapsed(initialExtensionsCollapsed(), false);
      return;
    }
    var panel = document.getElementById('extpanel-body');
    var wasOpen = wasDrawer && panel && panel.classList.contains('drawer-open');
    setExtensionsCollapsed(wasOpen ? false : currentExtensionsCollapsed(), false);
  }, 120);
});

(function () {
  var trigger = document.getElementById('extpanel-toggle');
  if (!trigger) return;
  trigger.addEventListener('click', function (e) {
    e.stopImmediatePropagation();
    e.preventDefault();
    toggleExtensions();
  }, true);
  trigger.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.stopImmediatePropagation();
    e.preventDefault();
    toggleExtensions();
  }, true);

  /* The scrim and Escape close the overlay only. On desktop the scrim is not in
     the layout and the body is a column, so neither can put away a rail the
     user chose — the same guard NetHub's own handlers carry. */
  var scrim = document.querySelector('.drawer-scrim');
  if (scrim) scrim.addEventListener('click', function () { setExtensionsCollapsed(true, false); });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (!extIsDrawer()) return;
    var body = document.getElementById('extpanel-body');
    if (!body || !body.classList.contains('drawer-open')) return;
    e.stopImmediatePropagation();
    setExtensionsCollapsed(true, false);
  }, true);
})();

setExtensionsCollapsed(initialExtensionsCollapsed(), false);

/* The socket frame is the panel's usual painter, but it only carries the key
   when a reconciler is running — a Termcp started without one sends nothing,
   and the column is in the markup whether or not anything answers. This fills
   that gap once: it runs after the socket connects, paints nothing the frame
   has not already painted (_extRows stays null until someone does), and a
   failure still lands as an empty column rather than as an unrendered one. */
function loadExtensionsOnce() {
  if (_extRows) return;
  fetch(apiPath('/api/extensions'))
    .then(function (r) { return r.json(); })
    .then(function (j) { renderExtensionGrid(j.extensions || []); })
    .catch(function () { renderExtensionGrid([]); });
}

onLangChange(function () {
  if (_extRows) renderExtensionGrid(_extRows);
  extRepaintWindows();
});

/* ---- One card per extension: a window, and the configuration behind it --------
 * A card click opens the extension's card — its status — as a window, the kind a
 * session opens: several are open at once, they drag, tile and minimise together,
 * and the batch row's ▶ opens one for every ticked extension. The readout is the
 * reconciler's verdict, the handles an agent drives it through, which other
 * extensions share that connection, the tunnels it bound, the next retry, and the
 * failure history.
 *
 * The template itself lives behind the card's gear, in a modal — editing is one
 * thing at a time, and one TOML document is what it edits (removal included,
 * because removing a template is a template-level act). A create uses the same
 * modal with its name editable and a documented default seeded into the box.
 *
 * The window is a session window with the terminal taken out; the connecting
 * placeholder is the precedent. The chrome, drag, z-order, tile and touch
 * behaviour come from those classes, the body is ours, and it deliberately has no
 * session behind it: no _parentSid (a session frame must not close it), no
 * _placeholder (the Sessions plate must not grow a card for it), no _channels.
 * _sid carries the extension's name — which is what the tab bar labels it with —
 * and _extName is the identity every session lookup excludes it by (see
 * getShellWindowBySid): a name and a session id are both [A-Za-z0-9_-]{1,64}, so
 * _sid alone cannot tell the two kinds of window apart.
 */
var _extDialogName = null;   // extension the configuration modal is showing; null = a new one
var _extDetail = {};         // name -> failure history, for the open windows

function extModalEl(id) { return document.getElementById(id); }

/* The open card of one extension, found by _extName — the identity only these
 * windows carry. Looking it up by _sid was a collision waiting to happen: _sid
 * holds the extension's NAME, and a name is the author's to choose under the same
 * [A-Za-z0-9_-]{1,64} rule a session id is a valid 12-hex string under, so an
 * extension named after a copied id would have resolved to that session's terminal
 * window (and a delete would have closed it). One open card per extension. */
function extWinFor(name) {
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    if (wins[i]._extName === name) return wins[i];
  }
  return null;
}

/* openExtensionWindow opens (or raises) the card of one extension. pos is where
 * the click was, or null for the centred cascade a batch uses. */
function openExtensionWindow(name, pos) {
  var ex = extWinFor(name);
  if (ex) { focusShellWindow(ex); return ex; }
  var container = shellWindowsEl();
  if (!container) return null;

  shellWindowCount += 1;
  var win = document.createElement('div');
  /* ext-window is the card's own class: the shared chrome comes from .shell-window,
     and the size rule (as tall as what it says) hangs off this name. */
  win.className = 'shell-window ext-window';
  win.id = 'shell-window-' + shellWindowCount;
  win._sid = name;
  win._extName = name;
  win._connName = '';
  win._streamDone = true;   // there is no session behind this to watch
  win.setAttribute('data-ext-name', name);
  win.innerHTML =
    '<div class="shell-window-header">' +
      '<div class="shell-window-title-cluster">' +
        '<span class="shell-link-state"></span>' +
        '<h3 class="shell-window-title"><span class="sw-conn">' + escapeHtml(name) + '</span></h3>' +
      '</div>' +
      '<div class="shell-window-header-btns">' +
        '<button type="button" class="conn-edit-btn ext-win-config" title="Edit profile" data-i18n-title="conn.aria.editProfile" aria-label="Edit profile" data-i18n-aria="conn.aria.editProfile">' + SVG_CONN_EDIT + '</button>' +
        SHELL_WINDOW_MAX_HTML +
        SHELL_WINDOW_MIN_HTML_CARD +
        '<button type="button" class="close-btn" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg></button>' +
      '</div>' +
    '</div>' +
    '<div class="shell-window-content"><div class="ext-win-body">' +
      '<div class="ext-status-head">' +
        '<span class="ext-status-word"></span>' +
        '<span class="ext-status-meta"></span>' +
      '</div>' +
      '<div class="ext-status-grid"></div>' +
      '<div class="ext-status-fails"></div>' +
      '<div class="modal-actions">' +
        '<button type="button" class="btn ext-win-toggle"></button>' +
        '<button type="button" class="btn btn-primary ext-win-retry" title="Retry now" data-i18n-title="ext.retry.title"><span data-i18n="ext.retry.title">Retry now</span></button>' +
      '</div>' +
    '</div>' +
    SHELL_RESIZE_HANDLES_HTML +
    '</div>';

  applyI18n(win);
  var header = win.querySelector('.shell-window-header');
  positionShellWindowFromClick(win, pos);
  container.appendChild(win);
  /* Placement ran on the session default (640x480) and a card is as big as what it
     says, so neither placement dimension is kept. BOTH overrides are desktop-only:
     on touch the window is .win-fullscreen, whose rule owns 100vw x 100dvh, and an
     inline width/height beats a media query — writing either here would shrink a
     full-screen card to its content (see the note on that rule in terminal.css). */
  if (!isMobileViewport()) {
    win.style.width = 'max-content';
    win.style.height = 'auto';
    /* The box has to be in the document and measured before it can be re-centred on
       the placement's centre lines. */
    recentreShellWindowForSize(win);
  }
  /* The card stays on those centre lines while its own numbers arrive — the failure
     history is fetched after the window exists. Grabbing the header or a grip hands
     the geometry over for good: a card the reader has moved (or sized) is theirs. */
  [header].concat(Array.prototype.slice.call(win.querySelectorAll('.shell-window-resize-handle')))
    .forEach(function (el) {
      if (el) el.addEventListener('pointerdown', function () { win._extTaken = true; }, true);
    });
  bindShellWindowMouseToFront(win);
  bindShellWindowMinButton(win, win.querySelector('.shell-window-min-btn'));
  bindShellWindowMaxButton(win);
  setupShellWindowDrag(win, header);
  setupShellWindowResize(win);
  setupMobileSessionSwitcher(win);
  bringShellWindowToFront(win);
  if (_layoutMode === 'tile') { tileWindow(win); openPaneWorkspace(); }
  else { _layoutMaybeAutoTile(); }
  if (_winsHidden) showAllWindows();

  /* This window's close is its own: there is no session to archive, so it must not
     reach the close-session dialog the shared binding offers. */
  var closeBtn = win.querySelector('.close-btn');
  if (closeBtn) closeBtn.addEventListener('click', function (e) { e.stopPropagation(); closeShellWindow(win); });
  var cfg = win.querySelector('.ext-win-config');
  if (cfg) cfg.addEventListener('click', function (e) { e.stopPropagation(); openExtensionDialog(name); });
  var toggle = win.querySelector('.ext-win-toggle');
  if (toggle) toggle.addEventListener('click', function () {
    var row = extRowByName(name);
    setExtensionEnabled(name, !(row && row.enabled), toggle);
  });
  var retry = win.querySelector('.ext-win-retry');
  if (retry) retry.addEventListener('click', function () { retryExtension(name, retry); });

  paintExtensionWindow(win, name);
  loadExtensionHistory(name);
  return win;
}

/* paintExtensionWindow is the card's readout, painted from the frame the panel
 * already has: a status transition repaints every open card, the same way it
 * repaints the grid behind them. */
function paintExtensionWindow(win, name) {
  var row = extRowByName(name);
  var body = win && win.querySelector('.ext-win-body');
  if (!body) return;
  if (!row) {
    body.innerHTML = '<p class="ext-empty-text">' + escapeHtml(t('ext.status.gone')) + '</p>';
    return;
  }
  /* The dot: is-live for a working extension, and the neutral base style for
     everything else — there is no is-idle rule, and the panel's own word sits
     right beside it. The shared helper's fallback tip is session vocabulary
     ("Running"), which would be a lie on an extension that is down, so the tip
     is this panel's own label instead. */
  setWindowLinkState(win, row.status === 'available' ? 'is-live' : 'is-idle');
  var dot = win.querySelector('.shell-link-state');
  if (dot) { dot.title = extStatusLabel(row.status); dot.setAttribute('aria-label', extStatusLabel(row.status)); }
  var word = win.querySelector('.ext-status-word');
  if (word) word.textContent = extStatusLabel(row.status);
  var meta = win.querySelector('.ext-status-meta');
  if (meta) {
    meta.textContent = [extKindLabel(row.kind), row.target, row.enabled ? '' : t('ext.status.parked')]
      .filter(function (x) { return !!x; }).join(' · ');
  }

  var grid = win.querySelector('.ext-status-grid');
  if (grid) {
    var rows = '';
    if (row.description) rows += extStatusRow(t('ext.modal.description'), row.description, false);
    /* A url template's endpoint is what it talks to, and the one field that says
       which of the two mcp shapes this is; the kind label alone cannot. */
    if (row.url) rows += extStatusRow(t('ext.status.url'), row.url, false);
    rows += extStatusRow(t('ext.status.session'), row.session_id, true);
    rows += extStatusRow(t('ext.status.shell'), row.shell_id, true);
    (row.forwards || []).forEach(function (f) {
      rows += extStatusRow(extForwardLabel(f.kind),
        [f.listen_addr, f.target_addr].filter(function (x) { return !!x; }).join(' → '), false);
    });
    if (row.next_retry_ms) {
      var secs = Math.max(0, Math.round((row.next_retry_ms - Date.now()) / 1000));
      rows += extStatusRow(t('ext.status.nextRetry'), t('ext.status.inSeconds', { count: secs }), false);
    }
    var shared = extSharingNote(row);
    if (shared) rows = '<p class="conn-check-hint">' + escapeHtml(shared) + '</p>' + rows;
    grid.innerHTML = rows;
    grid.querySelectorAll('.ext-copy-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        copyTextToClipboard(btn.getAttribute('data-copy')).then(function () {
          showCopyToast();
        }).catch(function () {
          showCopyToast(t('toast.copy.failed'));
        });
      });
    });
  }

  var fails = win.querySelector('.ext-status-fails');
  if (fails) {
    var history = _extDetail[name] || [];
    if (!history.length) {
      fails.innerHTML = '';
    } else {
      fails.innerHTML = '<div class="ext-status-sub">' + escapeHtml(t('ext.status.failures')) + '</div>' +
        history.slice().reverse().map(function (f) {
          return '<div class="ext-fail-item"><span class="ext-fail-stage">' + escapeHtml(f.stage || '') + '</span>' +
            '<span class="ext-fail-time">' + escapeHtml(fmtTime(f.at)) + '</span>' +
            '<span class="ext-fail-reason">' + escapeHtml(f.reason || '') + '</span></div>';
        }).join('');
    }
  }

  var retryBtn = win.querySelector('.ext-win-retry');
  if (retryBtn) retryBtn.disabled = !(row.status === 'unavailable' || row.status === 'probing');
  var toggleBtn = win.querySelector('.ext-win-toggle');
  if (toggleBtn) toggleBtn.textContent = row.enabled ? t('ext.status.disable') : t('ext.status.enable');
  /* A taller readout is still the card the click asked for, in the middle. */
  if (!win._extTaken) recentreShellWindowForSize(win);
}

/* loadExtensionHistory reads the failure history the frame does not carry. The
 * card is already up — the row it was opened from is the verdict — so a failed
 * read costs the history, not the window. */
function loadExtensionHistory(name) {
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name))
    .then(function (r) {
      if (!r.ok) throw new Error(r.status);
      return r.json();
    })
    .then(function (j) {
      _extDetail[name] = j.failures || [];
      var win = extWinFor(name);
      if (win) paintExtensionWindow(win, name);
    })
    .catch(function () {});
}

/* Every open card repaints from the frame, and from a language switch. */
function extRepaintWindows() {
  allShellWins().forEach(function (w) {
    if (w._extName) paintExtensionWindow(w, w._extName);
  });
}

/* openExtensionDialog is the configuration, and only that: a create (name null,
 * the name editable, a documented default in the box) or an edit of a stored
 * template, which is the TOML file the store holds. */
function openExtensionDialog(name) {
  var modal = extModalEl('modal-ext');
  if (!modal) return;
  _extDialogName = name || null;
  showExtError('');
  var title = extModalEl('modal-ext-title');
  if (title) title.textContent = name || t('ext.modal.title');
  var nameEl = extModalEl('ext-f-name');
  if (nameEl) {
    nameEl.readOnly = !!name;
    nameEl.value = name || '';
  }
  /* Removal is a stored template's business: a create has nothing to remove. */
  var delBtn = extModalEl('modal-ext-delete');
  if (delBtn) delBtn.style.display = name ? '' : 'none';
  modal.classList.remove('hidden');
  if (name) {
    loadExtensionToml(name);
  } else {
    var area = extModalEl('ext-f-toml');
    if (area) area.value = extTomlSeed();
    if (nameEl) nameEl.focus();
  }
}

function closeExtensionDialog() {
  var modal = extModalEl('modal-ext');
  if (modal) modal.classList.add('hidden');
  _extDialogName = null;
  showExtError('');
}

function showExtError(msg) {
  var err = extModalEl('modal-ext-err');
  if (!err) return;
  err.textContent = msg || '';
  err.style.display = msg ? 'block' : 'none';
}

/* ---- The card's readout ----------------------------------------------------- */

/* A labelled row of the card. The value is monospace because most of them are
   identifiers an author copies into a tool call. */
function extStatusRow(label, value, copyable) {
  if (!value) return '';
  var copy = copyable
    ? '<button type="button" class="ext-copy-btn" data-copy="' + escapeHtml(value) + '" title="' +
      escapeHtml(t('ext.status.copy')) + '" aria-label="' + escapeHtml(t('ext.status.copy')) + '">' + SVG_COPY_12 + '</button>'
    : '';
  return '<div class="ext-status-row"><span class="ext-status-label">' + escapeHtml(label) + '</span>' +
    '<span class="ext-status-value">' + escapeHtml(value) + '</span>' + copy + '</div>';
}

/* Other extensions served by the same connection. The session is shared per host,
   and saying so is what explains why an agent's shell_id is not the session the
   listing names. */
function extSharingNote(row) {
  if (!row.session_id) return '';
  var names = (_extRows || []).filter(function (e) {
    return e && e.session_id === row.session_id && e.name !== row.name;
  }).map(function (e) { return e.name; });
  if (!names.length) return '';
  return t('ext.status.shared', { names: names.join(', ') });
}

/* ---- The configuration page ------------------------------------------------- */

/* loadExtensionToml reads the file the store holds, byte for byte: what the author
 * edits here is what an export hands out and what a colleague's file would
 * contain. */
function loadExtensionToml(name) {
  var area = extModalEl('ext-f-toml');
  if (area) area.value = '';
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name) + '?format=toml')
    .then(function (r) {
      if (!r.ok) throw new Error(r.status);
      return r.text();
    })
    .then(function (text) {
      if (_extDialogName === name && area) area.value = text;
    })
    .catch(function () {
      showExtError(t('ext.modal.loadFailed'));
    });
}

/* The document a create starts from: the two fields a template cannot do without,
 * the conventional stdio server, and a line of comment per field — a person who
 * has never written one of these should be able to read what to change, which is
 * the whole reason this is a document and not a form. */
function extTomlSeed() {
  return [
    '# A termcp extension. kind + target is enough; every other field is an override.',
    'kind = "mcp"',
    '# target: an ssh_config profile name (ssh_config action=list), or "internal"',
    'target = "internal"',
    '',
    '# What it runs. A pipe channel starts this command and speaks line-delimited',
    '# JSON-RPC to it; pty is an interactive terminal instead; empty is no shell at',
    '# all (SFTP and forwards only).',
    'mode = "pipe"',
    'command = "npx"',
    'args = ["-y", "example-mcp-server"]',
    '',
    '# Or a remote MCP server reached over HTTP instead of a program to run: the',
    '# relay runs inside termcp, so nothing is installed anywhere and target must be',
    '# "internal". A path ending in /sse selects the SSE transport. `url` replaces',
    '# `command`/`args` — set one or the other, not both.',
    '# url = "https://mcp.example.com/mcp"',
    '# headers = { X-Api-Key = "..." }',
    '',
    '# What the agent driving it may call (a whitelist; empty = the full surface).',
    'allow = ["shell_input", "shell_key", "shell_output"]',
    '',
    '# Read by the agent before it uses this extension: the part that is not obvious',
    '# from the fields above.',
    '# notes = "..."',
    ''
  ].join('\n');
}

/* extSaveTemplate saves what the textarea holds. The name is the directory the
 * template lives in, so it rides in the query on a create and in the path on an
 * update — the file itself has no place for it. */
function extSaveTemplate() {
  var name = _extDialogName;
  var forCreate = !name;
  var area = extModalEl('ext-f-toml');
  var body = area ? area.value : '';
  if (!body.trim()) {
    showExtError(t('ext.modal.tomlEmpty'));
    return;
  }
  var query = '';
  if (forCreate) {
    var nameEl = extModalEl('ext-f-name');
    var newName = nameEl ? nameEl.value.trim() : '';
    if (!newName) {
      showExtError(t('ext.modal.nameRequired'));
      return;
    }
    query = '?name=' + encodeURIComponent(newName);
  }
  var saveBtn = extModalEl('modal-ext-save');
  if (saveBtn) saveBtn.disabled = true;
  var url = forCreate
    ? apiPath('/api/extensions') + query
    : apiPath('/api/extensions/') + encodeURIComponent(name);
  fetch(url, {
    method: forCreate ? 'POST' : 'PUT',
    headers: { 'Content-Type': 'application/toml; charset=utf-8' },
    body: body
  }).then(function (r) {
    if (saveBtn) saveBtn.disabled = false;
    if (!r.ok) {
      return r.text().then(function (txt) {
        throw new Error(txt || r.status);
      });
    }
    /* The dialog closes either way: the next frame repaints the card, and the
       card is where the result of an edit is read. */
    closeExtensionDialog();
  }).catch(function (err) {
    if (saveBtn) saveBtn.disabled = false;
    showExtError(t('ext.modal.saveFailed') + ' ' + ((err && err.message) || err));
  });
}

/* Park or wake a template. The reconciler does the rest: a parked extension's
 * channel and tunnels are closed, an enabled one is brought up again. */
function setExtensionEnabled(name, enabled, btn) {
  if (!name) return;
  if (btn) btn.disabled = true;
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name), {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled: enabled })
  }).then(function (r) {
    if (btn) btn.disabled = false;
    if (!r.ok) return r.text().then(function (txt) { throw new Error(txt || r.status); });
  }).catch(function (err) {
    if (btn) btn.disabled = false;
    showCopyToast(t('ext.toast.saveFailed', { msg: String((err && err.message) || err) }));
  });
}

/* Delete is a confirm, because the template is a file but the extension is a
 * connection the reconciler was holding warm — the author is removing both. */
function deleteOneExtension(name, win) {
  if (!name) return;
  confirmDialog({
    title: t('ext.delete.title'),
    message: t('ext.delete.confirm', { name: name })
  }).then(function (ok) {
    if (!ok) return;
    fetch(apiPath('/api/extensions/') + encodeURIComponent(name), { method: 'DELETE' })
      .then(function (r) {
        if (!r.ok && r.status !== 204) {
          return r.text().then(function (txt) { throw new Error(txt || r.status); });
        }
        if (win) closeShellWindow(win);
        closeExtensionDialog();
      })
      .catch(function (err) {
        showCopyToast(t('toast.delete.failed', { msg: String((err && err.message) || err) }));
      });
  });
}

/* ---- Import ----------------------------------------------------------------
 * The dialog and the readout are the connection import's, over the extension
 * document: one file, no overwrites, and the names that had to move are listed
 * because that is the only thing the author has to be told about. */
(function () {
  var open = document.getElementById('ext-import-open');
  if (!open) return;
  open.addEventListener('click', function (e) {
    e.stopPropagation();
    var btn = document.getElementById('ext-import-run');
    if (btn) btn.disabled = false;
    var file = document.getElementById('ext-import-file');
    if (file) file.value = '';
    var err = document.getElementById('ext-import-err');
    if (err) { err.style.display = 'none'; err.textContent = ''; }
    var out = document.getElementById('ext-import-result');
    if (out) { out.style.display = 'none'; out.textContent = ''; }
    showModal('modal-ext-import');
  });
  var close = document.getElementById('ext-import-close');
  if (close) close.addEventListener('click', function () { hideModal('modal-ext-import'); });
  var run = document.getElementById('ext-import-run');
  if (run) run.addEventListener('click', function () {
    var input = document.getElementById('ext-import-file');
    var err = document.getElementById('ext-import-err');
    var out = document.getElementById('ext-import-result');
    if (err) err.style.display = 'none';
    if (out) out.style.display = 'none';
    var file = input && input.files && input.files[0];
    if (!file) {
      if (err) { err.textContent = t('conn.batch.selectFile'); err.style.display = 'block'; }
      return;
    }
    var btn = this;
    btn.disabled = true;
    // The file is opaque to the UI; the backend parses and validates its TOML.
    fetch(apiPath('/api/extensions/batch'), {
      method: 'POST', headers: { 'Content-Type': 'application/toml' }, body: file
    }).then(function (r) {
      if (!r.ok) return r.text().then(function (message) { throw new Error(message || String(r.status)); });
      return r.json();
    }).then(function (result) {
      if (result.renamed && result.renamed.length) {
        var lines = [t('ext.batch.imported', { count: result.imported })];
        result.renamed.forEach(function (entry) {
          lines.push(t('conn.batch.renamed', { from: entry.from, to: entry.to }));
        });
        if (out) {
          out.textContent = lines.join('\n');
          out.style.display = 'block';
        }
        if (input) input.value = '';
      } else {
        hideModal('modal-ext-import');
        showCopyToast(t('ext.batch.imported', { count: result.imported }));
      }
    }).catch(function (e) {
      if (err) { err.textContent = String((e && e.message) || e); err.style.display = 'block'; }
    }).finally(function () { btn.disabled = false; });
  });
})();
/* ---- Panel wiring ---------------------------------------------------------- */
(function () {
  var add = document.getElementById('ext-add');
  if (add) {
    add.addEventListener('click', function () { openExtensionDialog(null); });
    add.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      e.preventDefault();
      openExtensionDialog(null);
    });
  }

  var modal = extModalEl('modal-ext');
  if (!modal) return;
  extModalEl('modal-ext-close').addEventListener('click', closeExtensionDialog);
  extModalEl('modal-ext-cancel').addEventListener('click', closeExtensionDialog);
  extModalEl('modal-ext-save').addEventListener('click', extSaveTemplate);
  var modalDelete = extModalEl('modal-ext-delete');
  if (modalDelete) modalDelete.addEventListener('click', function () {
    var name = _extDialogName;
    if (!name) return;
    deleteOneExtension(name, extWinFor(name));
  });
  modal.addEventListener('click', function (e) {
    if (e.target === modal) closeExtensionDialog();
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (!modal.classList.contains('hidden')) {
      e.stopImmediatePropagation();
      closeExtensionDialog();
    }
  }, true);
})();
