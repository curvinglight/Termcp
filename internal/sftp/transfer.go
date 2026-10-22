package sftp

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/encoding"
)

// StreamReadTo reads from remotePath (optionally at offset for length bytes) and streams
// raw bytes into w. Uses io.CopyN for zero-buffer streaming — never loads the whole file
// into memory. For large files use offset=0, length<=0 to stream the entire file.
func (s *Client) StreamReadTo(w io.Writer, remotePath string, offset, length int64) (int64, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat remote file: %w", err)
	}

	if offset < 0 {
		offset = 0
	}
	if length <= 0 || offset+length > fi.Size() {
		length = fi.Size() - offset
	}
	if length < 0 {
		length = 0
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}

	return io.CopyN(w, f, length)
}

// StreamWriteFrom reads raw bytes from r and writes them to remotePath at the given
// offset. A nil offset truncates the file; a pointer to 0 overwrites from the first
// byte without truncating. Uses io.Copy to stream without buffering in memory.
func (s *Client) StreamWriteFrom(r io.Reader, remotePath string, offset *int64) (int64, error) {
	flag, off := writeOpenFlags(offset)
	f, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}

	return io.Copy(f, r)
}

// maxInlineRead caps how many bytes a single text/hex read pulls into memory.
// The file is still fully reachable: callers page with offset + has_more.
// mode=file streams to a local file and is intentionally uncapped.
const maxInlineRead = 8 << 20 // 8 MiB

// normalizeReadRange clamps a requested byte window to [0, totalSize] and to an
// optional in-memory limit. It is overflow-safe by construction: it never
// computes offset+length, so an enormous length (or a huge/negative offset)
// can never yield a negative or out-of-range allocation in make([]byte, length).
// length <= 0 means "the rest of the file".
func normalizeReadRange(totalSize, offset, length, limit int64) (int64, int64) {
	if totalSize < 0 { // some SFTP servers report -1 for special files
		totalSize = 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > totalSize {
		offset = totalSize
	}
	remaining := totalSize - offset // >= 0
	if length <= 0 || length > remaining {
		length = remaining
	}
	if limit > 0 && length > limit {
		length = limit
	}
	if length < 0 {
		length = 0
	}
	return offset, length
}

// ReadFile reads a file (or segment) from the remote.
func (s *Client) ReadFile(remotePath string, offset, length int64, mode string, localPath string) (*FileResult, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat remote file: %w", err)
	}
	// A DIRECTORY IS A CALLER MISTAKE, NOT A TRANSPORT FAULT, and it has to be
	// caught HERE. Reading one through SFTP fails deep inside the reader and comes
	// back as `sftp: "read /tmp/x: is a directory" (SSH_FX_FAILURE)` — an
	// ErrSSHFxFailure indistinguishable from a genuine fault, complete with the
	// protocol's own wording. Stat already knows the answer, so say it in terms the
	// caller can act on.
	if fi.IsDir() {
		return nil, fmt.Errorf("%s: %w; use --action stat to list its contents", remotePath, ErrIsDirectory)
	}
	totalSize := fi.Size()

	inlineCap := int64(maxInlineRead)
	if mode == "file" {
		inlineCap = 0 // io.CopyN bounds itself to the file; no memory blow-up
	}
	offset, length = normalizeReadRange(totalSize, offset, length, inlineCap)

	result := &FileResult{
		Mode:      mode,
		TotalSize: totalSize,
		Offset:    offset,
		Length:    length,
		HasMore:   offset+length < totalSize,
	}

	// File mode: write to local file.
	if mode == "file" {
		if localPath == "" {
			return nil, fmt.Errorf("local_path required for file mode")
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek: %w", err)
		}
		localFile, err := os.Create(localPath)
		if err != nil {
			return nil, fmt.Errorf("create local file: %w", err)
		}
		defer localFile.Close()
		n, err := io.CopyN(localFile, f, length)
		if err != nil {
			return nil, fmt.Errorf("copy to local: %w", err)
		}
		result.BytesRead = n
		result.LocalPath = localPath
		return result, nil
	}

	// Read the segment.
	buf := make([]byte, length)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek: %w", err)
	}
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, fmt.Errorf("read: %w", err)
	}
	segment := buf[:n]
	result.BytesRead = int64(n)
	result.Length = int64(n)

	// Encode based on mode.
	switch mode {
	case "hex":
		body := fmt.Sprintf("%x", segment)
		result.Data = &body
	case "text", "":
		body := encoding.EncodeText(segment)
		result.Data = &body
	default:
		return nil, fmt.Errorf("unknown mode %q (use text, hex, or file)", mode)
	}

	return result, nil
}

// WriteFile writes data to a remote file, either inline or from a local file.
func (s *Client) WriteFile(remotePath string, offset *int64, data string, mode string, localPath string, localOffset, length int64) (int64, error) {
	if localPath != "" {
		return s.writeFromLocal(remotePath, offset, localPath, localOffset, length)
	}
	return s.writeInline(remotePath, offset, data, mode)
}

func (s *Client) writeInline(remotePath string, offset *int64, data string, mode string) (int64, error) {
	var raw []byte
	switch mode {
	case "hex":
		decoded, err := encoding.HexDecode(data)
		if err != nil {
			return 0, fmt.Errorf("hex decode: %w", err)
		}
		raw = decoded
	case "text", "":
		raw = encoding.DecodeText(data)
	default:
		return 0, fmt.Errorf("unknown mode %q", mode)
	}

	flag, off := writeOpenFlags(offset)
	f, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	// SEEK ONLY WHEN NEEDED, BUT ALWAYS KNOW WHERE WE ARE. The file's own offset
	// starts at 0, so writing at 0 needs no seek; writing anywhere else does. This
	// is the half of the offset-0 bug that made it invisible: the flag was right
	// and the position was right, so only the truncate decision was wrong.
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}
	n, err := f.Write(raw)
	if err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}
	return int64(n), nil
}

func (s *Client) writeFromLocal(remotePath string, offset *int64, localPath string, localOffset, length int64) (int64, error) {
	localFile, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("open local file: %w", err)
	}
	defer localFile.Close()

	fi, err := localFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat local file: %w", err)
	}
	if length <= 0 || localOffset+length > fi.Size() {
		length = fi.Size() - localOffset
	}
	if _, err := localFile.Seek(localOffset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek local: %w", err)
	}

	flag, off := writeOpenFlags(offset)
	remoteFile, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer remoteFile.Close()

	if off > 0 {
		if _, err := remoteFile.Seek(off, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek remote: %w", err)
		}
	}

	n, err := io.CopyN(remoteFile, localFile, length)
	if err != nil {
		return 0, fmt.Errorf("copy: %w", err)
	}
	return n, nil
}

// writeOpenFlags decides how a write opens its target.
//
// A POINTER, NOT A SENTINEL: nil means the caller did not specify an offset (the
// file is truncated and rewritten from the start), while a pointer to 0 means
// "overwrite from the first byte, preserving the rest". They used to share the
// value 0 under `offset <= 0`, which made the destructive one the default.
//
// The returned offset is always non-negative: 0 for the truncate case, and
// whatever the caller passed otherwise.
func writeOpenFlags(offset *int64) (int, int64) {
	flag := os.O_RDWR | os.O_CREATE
	if offset == nil {
		return flag | os.O_TRUNC, 0
	}
	off := *offset
	if off < 0 {
		off = 0
	}
	return flag, off
}

// maxReplaceSize bounds how large a file ReplaceInFile will load into memory.
// An exact-replace primitive is for editing text, not for rewriting a 4 GB log:
// loading the latter would OOM the daemon rather than edit it. Callers with
// bigger files have file write + offsets.
const maxReplaceSize = 16 << 20 // 16 MiB

// ReplaceInFile replaces exactly one occurrence of oldText with newText.
//
// THE UNIQUENESS CONSTRAINT IS THE FEATURE. A replace that "just replaces" is how
// an edit lands in the wrong place: an agent asked to change `timeout = 5` in a
// config with three such lines would silently pick the first, and the mistake is
// invisible until the wrong service misbehaves. Matching zero or several times is
// an error that names the count, so the caller narrows the pattern instead of
// guessing which one it got.
//
// ATOMIC ON POSIX: the result is written to a temporary file in the SAME directory
// and renamed over the original. rename(2) within a filesystem is atomic, so a
// reader either sees the old file or the new one — never a half-written one, which
// is what an in-place write produces if the process dies mid-way.
//
// Returns the number of bytes written (the new file's size).
func (s *Client) ReplaceInFile(remotePath, oldText, newText string) (int64, error) {
	if oldText == "" {
		return 0, fmt.Errorf("old_text must not be empty")
	}
	f, err := s.client.Open(remotePath)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat remote file: %w", err)
	}
	if fi.IsDir() {
		return 0, fmt.Errorf("%s: %w; use --action stat to list its contents", remotePath, ErrIsDirectory)
	}
	if fi.Size() > maxReplaceSize {
		return 0, fmt.Errorf("%s is %d bytes, over the %d-byte limit for exact replace; use file read + file write with --offset for a file this large",
			remotePath, fi.Size(), maxReplaceSize)
	}

	original, err := io.ReadAll(io.LimitReader(f, maxReplaceSize+1))
	if err != nil {
		return 0, fmt.Errorf("read remote file: %w", err)
	}
	// CLOSE BEFORE RENAMING OVER IT. The deferred Close above runs when the function
	// returns, which is after the rename — fine on POSIX, where an open handle does not
	// block a rename, and fatal on Windows, where the rename and then the remove both
	// fail with "The process cannot access the file because it is being used by another
	// process". Closing here (and tolerating the second Close from the defer) makes the
	// handle's lifetime match the read that needs it.
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("close remote file: %w", err)
	}

	count := strings.Count(string(original), oldText)
	if count == 0 {
		return 0, fmt.Errorf("%w: %q does not appear in %s", ErrPatternNotFound, previewForError(oldText), remotePath)
	}
	if count > 1 {
		return 0, fmt.Errorf("%w: %q appears %d times in %s; include more surrounding context so it matches once",
			ErrPatternAmbiguous, previewForError(oldText), count, remotePath)
	}

	updated := strings.Replace(string(original), oldText, newText, 1)

	// Temporary file in the same directory, so the rename stays within one
	// filesystem and is therefore atomic.
	tmpPath := siblingOf(remotePath, ".termcp-replace-"+randomSuffix())

	if err := s.writeWholeFile(tmpPath, []byte(updated)); err != nil {
		_ = s.RemoveFile(tmpPath)
		return 0, fmt.Errorf("write temporary file: %w", err)
	}
	// Preserve the mode: a replace must not silently change permissions, which is
	// exactly what a freshly created temp file would otherwise do (0644).
	if err := s.client.Chmod(tmpPath, fi.Mode().Perm()); err != nil {
		_ = s.RemoveFile(tmpPath)
		return 0, fmt.Errorf("preserve permissions on temporary file: %w", err)
	}
	// RENAME OVER THE TARGET IS NOT PORTABLE. On POSIX the rename replaces the
	// destination atomically, which is the whole reason for the temp file. Windows
	// answers SSH_FX_FAILURE "Access is denied" when the destination exists, so the
	// atomic swap is unavailable there and the best remaining sequence is remove then
	// rename — a window where the file is briefly absent, chosen deliberately over a
	// replace that simply does not work.
	if err := s.RenameFile(tmpPath, remotePath); err != nil {
		if rmErr := s.RemoveFile(remotePath); rmErr != nil {
			_ = s.RemoveFile(tmpPath)
			return 0, fmt.Errorf("rename temporary file over %s: %w (and removing the target first failed: %w)",
				remotePath, err, rmErr)
		}
		if err := s.RenameFile(tmpPath, remotePath); err != nil {
			_ = s.RemoveFile(tmpPath)
			return 0, fmt.Errorf("rename temporary file over %s after removing it: %w", remotePath, err)
		}
	}
	return int64(len(updated)), nil
}

// writeWholeFile creates (or truncates) remotePath and writes data.
func (s *Client) writeWholeFile(remotePath string, data []byte) error {
	f, err := s.client.Create(remotePath)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// previewForError shortens a pattern for an error message. A replace pattern can be
// a whole paragraph, and pasting it into an error would bury the count and the hint.
func previewForError(s string) string {
	const limit = 60
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// randomSuffix returns a short unique token for a temporary filename.
func randomSuffix() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand failing is not a reason to refuse the edit; the pid is enough
		// to avoid a collision within one process.
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// siblingOf builds a hidden name beside a REMOTE path, keeping the path's own
// spelling.
//
// `path.Dir` is the wrong tool: it only knows "/". A Windows target hands over
// `C:\Users\x\agent.conf`, which path.Dir reports as ("." , whole-string) — so the
// temporary file came out as `.C:\Users\x\agent.conf.termcp-replace-…`, a name the
// server rejected. The remote path is not a local path and cannot be parsed with the
// local platform's rules.
//
// Both separators are honoured and the found one is reused, so the name is built the
// way the caller spelled it. Normalising to "/" would work for a POSIX server and
// break a Windows one; guessing from the string alone is the only option, and echoing
// the caller's own spelling back is the least surprising answer.
func siblingOf(remotePath, suffix string) string {
	i := strings.LastIndexAny(remotePath, `/\`)
	if i < 0 {
		return "." + remotePath + suffix
	}
	dir, base, sep := remotePath[:i], remotePath[i+1:], remotePath[i:i+1]
	if dir == "" {
		// "/x" -> the root; keep the separator so the path stays absolute.
		return sep + "." + base + suffix
	}
	return dir + sep + "." + base + suffix
}
