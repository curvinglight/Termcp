/**
 * Update notice.
 *
 * The page asks GitHub for the latest release and, if this build is an older
 * release, offers a link to it. Three rules keep that from being noise:
 *
 *  - Only a real release is compared. `make` labels every non-tagged build
 *    `dev-<sha>` (and `dev-<sha>-dirty`), none of which has a position in a
 *    release order; telling those to update would fire on every local build and
 *    be wrong every time. `versionParts` returns null for anything that is not
 *    plain `vX.Y.Z`, and every caller treats null as "say nothing".
 *  - The comparison is numeric, per component. Compared as strings, "0.2.10" is
 *    older than "0.2.9", which is exactly the release after the next one.
 *  - A dismissed notice stays dismissed for that version, so it cannot nag on
 *    every page load; the next release asks again.
 *
 * The lookup is best-effort and entirely optional: no release, a rate limit, no
 * network, an ad-blocked api.github.com — all of them leave the page exactly as
 * it was. Nothing else in the UI depends on this file.
 */
var UPDATE_RELEASES_URL = 'https://api.github.com/repos/open-mcp-ai/Termcp/releases';
var UPDATE_DISMISSED_KEY = 'termcp.update.dismissed';

/** Split a release version into numeric parts, or null when it is not one.
 *  Only `vX.Y.Z` counts: a pre-release (`-rc.1`) or a dev build is deliberately
 *  excluded, because neither can be ordered against a release. */
function versionParts(value) {
  var match = /^v?(\d+)\.(\d+)\.(\d+)$/.exec(String(value == null ? '' : value).trim());
  if (!match) return null;
  return [parseInt(match[1], 10), parseInt(match[2], 10), parseInt(match[3], 10)];
}

/** Compare two release versions: -1, 0 or 1, or null when either side is not a
 *  release. Null is not "equal" — it means the question does not apply. */
function compareVersions(a, b) {
  var left = versionParts(a);
  var right = versionParts(b);
  if (!left || !right) return null;
  for (var i = 0; i < 3; i++) {
    if (left[i] !== right[i]) return left[i] < right[i] ? -1 : 1;
  }
  return 0;
}

/** What this instance is running, in a form that can be ordered against a
 *  release: the numeric triple, plus whether it is a pre-release of it.
 *
 *  `current` is allowed a pre-release suffix (`v0.2.8-rc.1`), which `versionParts`
 *  rejects for the release side. An rc build has a real position in the order —
 *  it is the release it precedes, minus the final — so someone running rc.1 must
 *  be told when the final lands. A dev build has no position at all, and gets
 *  null here like everywhere else. */
function currentVersionParts(value) {
  var text = String(value == null ? '' : value).trim();
  var release = versionParts(text);
  if (release) return { parts: release, prerelease: false };
  var pre = /^v?(\d+)\.(\d+)\.(\d+)-[0-9A-Za-z.-]+$/.exec(text);
  if (!pre) return null;
  return { parts: [parseInt(pre[1], 10), parseInt(pre[2], 10), parseInt(pre[3], 10)], prerelease: true };
}

/** The version to announce, or null when there is nothing to say.
 *  `current` is what this instance reports; `latest` is GitHub's newest release
 *  tag (null/'' when the lookup failed). Returns the bare version for display —
 *  the copy adds the `v`. */
function updateNoticeFor(current, latest) {
  var latestParts = versionParts(latest);
  var running = currentVersionParts(current);
  if (!latestParts || !running) return null;
  var newer = 0;
  for (var i = 0; i < 3 && newer === 0; i++) {
    if (latestParts[i] !== running.parts[i]) newer = latestParts[i] > running.parts[i] ? 1 : -1;
  }
  /* Same numbers and the build is a pre-release: the final is an upgrade over
     rc.N, which is the whole point of running one. */
  if (newer === 0 && running.prerelease) newer = 1;
  if (newer !== 1) return null;
  return String(latest).trim().replace(/^v/, '');
}

/** Release page for a version, for the notice's link. */
function updateReleaseURL(version) {
  return 'https://github.com/open-mcp-ai/Termcp/releases/tag/v' + String(version).replace(/^v/, '');
}

function updateDismissed(version) {
  try { return localStorage.getItem(UPDATE_DISMISSED_KEY) === version; } catch (e) { return false; }
}

function dismissUpdateNotice(version) {
  try { localStorage.setItem(UPDATE_DISMISSED_KEY, version); } catch (e) {}
}

/** Mount the notice into the header, if there is one to mount.
 *
 *  Rendered as an <a> rather than in the toast stack: the toast stack is for
 *  things happening now, and this is a fact about the build that should stay
 *  readable until acted on or dismissed. */
function showUpdateNotice(version) {
  if (updateDismissed(version)) return;
  var host = document.querySelector('.app-header');
  if (!host || !host.parentNode) return;
  var existing = document.getElementById('update-notice');
  if (existing) existing.parentNode.removeChild(existing);

  var note = document.createElement('div');
  note.id = 'update-notice';
  note.className = 'update-notice';
  note.setAttribute('role', 'status');

  var link = document.createElement('a');
  link.className = 'update-notice-link';
  link.href = updateReleaseURL(version);
  link.target = '_blank';
  link.rel = 'noopener noreferrer';
  link.textContent = t('update.available', { version: version });
  note.appendChild(link);

  var close = document.createElement('button');
  close.type = 'button';
  close.className = 'update-notice-close';
  close.setAttribute('aria-label', t('update.dismiss'));
  close.textContent = '×';
  close.addEventListener('click', function () {
    dismissUpdateNotice(version);
    if (note.parentNode) note.parentNode.removeChild(note);
  });
  note.appendChild(close);

  /* Inside the header, after the nav: the header owns the top-right corner and
     an absolutely positioned notice would be the one element on the page that
     does not follow a phone layout. */
  host.appendChild(note);
}

/** Ask GitHub for the latest release and offer it when it is newer.
 *  Deliberately silent on every failure path — this is a convenience, never a
 *  thing the page reports an error about. */
function checkForUpdate(current) {
  if (!versionParts(current) || typeof fetch !== 'function') return Promise.resolve(null);
  return fetch(UPDATE_RELEASES_URL, { headers: { Accept: 'application/vnd.github+json' } })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (releases) {
      if (!Array.isArray(releases)) return null;
      /* The list is newest-first and includes drafts and pre-releases, neither of
         which is an upgrade target: a draft is unpublished, and an rc is newer
         than the release it precedes but not what a release user should move to.
         `releases` (not `releases/latest`) is used because the latter cannot see
         a pre-release at all, so an rc build would never be told the final
         shipped. */
      for (var i = 0; i < releases.length; i++) {
        var r = releases[i];
        if (!r || r.draft || r.prerelease) continue;
        var newer = updateNoticeFor(current, r.tag_name);
        if (newer) { showUpdateNotice(newer); return newer; }
        break;
      }
      return null;
    })
    .catch(function () { return null; });
}

checkForUpdate(window.__termcpVersion || '');
