package gc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// maxBlobSizeForPointerCheck caps which blobs we cat-file -p. Real
// LFS pointers are ~120-200 bytes; 1024 is generous and avoids
// reading multi-MB binary blobs into memory just to reject them.
const maxBlobSizeForPointerCheck = 1024

// BuildLiveSet enumerates every LFS object OID referenced by any
// reachable commit in the bare git repo at bareDir. Returns a set
// of lowercase 64-hex OIDs.
//
// Implementation:
//  1. git rev-list --objects --all → all reachable (oid, path) pairs.
//  2. git cat-file --batch-check '%(objectname) %(objecttype) %(objectsize)'
//     to filter to blobs ≤ maxBlobSizeForPointerCheck bytes.
//  3. git cat-file --batch '%(objectname)\n' to fetch each candidate
//     blob's bytes; pipe through ParsePointer.
//  4. Union pointed-to OIDs into the returned set.
//
// Two cat-file --batch invocations (vs spawning one process per blob)
// amortise git startup cost; the second call streams only small
// candidate blobs so total bytes read is bounded.
func BuildLiveSet(ctx context.Context, bareDir string) (map[string]struct{}, error) {
	candidates, err := listSmallBlobs(ctx, bareDir)
	if err != nil {
		return nil, fmt.Errorf("lfs/gc: listSmallBlobs: %w", err)
	}
	if len(candidates) == 0 {
		return map[string]struct{}{}, nil
	}
	bodies, err := readBlobs(ctx, bareDir, candidates)
	if err != nil {
		return nil, fmt.Errorf("lfs/gc: readBlobs: %w", err)
	}
	live := make(map[string]struct{})
	for _, body := range bodies {
		if oid, ok := ParsePointer(body); ok {
			live[oid] = struct{}{}
		}
	}
	return live, nil
}

// listSmallBlobs lists all reachable blob OIDs ≤ the size threshold
// via git rev-list piped into cat-file --batch-check.
func listSmallBlobs(ctx context.Context, bareDir string) (oids []string, retErr error) {
	// Pipe: rev-list --objects --all | awk '{print $1}' | cat-file --batch-check
	revList := exec.CommandContext(ctx, "git", "--no-replace-objects", "-C", bareDir,
		"rev-list", "--all", "--objects")
	revOut, err := revList.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rev-list stdout pipe: %w", err)
	}
	revErr := &bytes.Buffer{}
	revList.Stderr = revErr
	if err := revList.Start(); err != nil {
		return nil, fmt.Errorf("rev-list start: %w", err)
	}

	batchCheck := exec.CommandContext(ctx, "git", "--no-replace-objects", "-C", bareDir,
		"cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	bcIn, err := batchCheck.StdinPipe()
	if err != nil {
		_ = revOut.Close()
		_ = revList.Wait()
		return nil, fmt.Errorf("batch-check stdin pipe: %w", err)
	}
	bcOut, err := batchCheck.StdoutPipe()
	if err != nil {
		_ = bcIn.Close()
		_ = revOut.Close()
		_ = revList.Wait()
		return nil, fmt.Errorf("batch-check stdout pipe: %w", err)
	}
	bcErr := &bytes.Buffer{}
	batchCheck.Stderr = bcErr
	if err := batchCheck.Start(); err != nil {
		_ = bcIn.Close()
		_ = bcOut.Close()
		_ = revOut.Close()
		_ = revList.Wait()
		return nil, fmt.Errorf("batch-check start: %w", err)
	}

	// Feed rev-list output (first space-delimited token only) into batch-check.
	feedErr := make(chan error, 1)
	go func() {
		defer bcIn.Close()
		scanner := bufio.NewScanner(revOut)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if sp := strings.IndexByte(line, ' '); sp != -1 {
				line = line[:sp]
			}
			if line == "" {
				continue
			}
			if _, werr := io.WriteString(bcIn, line+"\n"); werr != nil {
				feedErr <- werr
				return
			}
		}
		feedErr <- scanner.Err()
	}()

	// Mirror readBlobs's cleanup invariant: on any error after both
	// subprocesses have started, kill + reap both so we don't leak.
	defer func() {
		if retErr == nil {
			return
		}
		if revList.Process != nil {
			_ = revList.Process.Kill()
		}
		if batchCheck.Process != nil {
			_ = batchCheck.Process.Kill()
		}
		select {
		case <-feedErr:
		default:
		}
		_ = revList.Wait()
		_ = batchCheck.Wait()
	}()

	bcReader := bufio.NewScanner(bcOut)
	bcReader.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for bcReader.Scan() {
		// Output line: "<oid> <type> <size>" or "<oid> missing".
		parts := strings.Fields(bcReader.Text())
		if len(parts) != 3 {
			continue
		}
		if parts[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || size > maxBlobSizeForPointerCheck {
			continue
		}
		oids = append(oids, parts[0])
	}

	if err := <-feedErr; err != nil {
		return nil, fmt.Errorf("rev-list scan: %w", err)
	}
	if err := revList.Wait(); err != nil {
		return nil, fmt.Errorf("rev-list wait: %w (stderr: %s)", err, revErr.String())
	}
	if err := bcReader.Err(); err != nil {
		return nil, fmt.Errorf("batch-check scan: %w", err)
	}
	if err := batchCheck.Wait(); err != nil {
		return nil, fmt.Errorf("batch-check wait: %w (stderr: %s)", err, bcErr.String())
	}
	return oids, nil
}

// readBlobs reads the bytes of each blob in oids via git cat-file --batch.
// Returns one []byte per input OID in the same order.
func readBlobs(ctx context.Context, bareDir string, oids []string) (bodies [][]byte, retErr error) {
	if len(oids) == 0 {
		return nil, nil
	}
	batch := exec.CommandContext(ctx, "git", "--no-replace-objects", "-C", bareDir,
		"cat-file", "--batch=%(objectname) %(objecttype) %(objectsize)")
	in, err := batch.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("batch stdin pipe: %w", err)
	}
	out, err := batch.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("batch stdout pipe: %w", err)
	}
	stderrBuf := &bytes.Buffer{}
	batch.Stderr = stderrBuf
	if err := batch.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, fmt.Errorf("batch start: %w", err)
	}

	feedErr := make(chan error, 1)
	go func() {
		defer in.Close()
		for _, oid := range oids {
			if _, werr := io.WriteString(in, oid+"\n"); werr != nil {
				feedErr <- werr
				return
			}
		}
		feedErr <- nil
	}()

	// Cleanup: on any error return below, kill the subprocess so the
	// feed goroutine's pending write fails fast rather than blocking
	// forever on a full OS pipe buffer. The deferred Wait drains the
	// process so we never leave a zombie.
	defer func() {
		if retErr != nil {
			if batch.Process != nil {
				_ = batch.Process.Kill()
			}
			// Drain the goroutine's send (channel is buffered cap=1, so
			// this won't block even if goroutine already wrote).
			select {
			case <-feedErr:
			default:
			}
			_ = batch.Wait()
		}
	}()

	bodies = make([][]byte, 0, len(oids))
	br := bufio.NewReader(out)
	for range oids {
		// Header line: "<oid> blob <size>\n" or "<oid> missing\n" if
		// the object disappeared between listSmallBlobs and now (e.g.,
		// concurrent mutation of the temp mirror — should not happen
		// since we own the bareDir, but surface clearly if it does).
		header, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("batch header read: %w (stderr: %s)", err, stderrBuf.String())
		}
		parts := strings.Fields(strings.TrimRight(header, "\n"))
		if len(parts) == 2 && parts[1] == "missing" {
			return nil, fmt.Errorf("lfs/gc: cat-file reports oid %s missing — mirror was mutated mid-walk", parts[0])
		}
		if len(parts) < 3 {
			return nil, fmt.Errorf("batch header malformed: %q", header)
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("batch header size: %w", err)
		}
		// Body is size bytes followed by a trailing '\n'.
		body := make([]byte, size)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, fmt.Errorf("batch body read: %w", err)
		}
		// Discard the trailing '\n'.
		if _, err := br.Discard(1); err != nil {
			return nil, fmt.Errorf("batch trailing nl: %w", err)
		}
		bodies = append(bodies, body)
	}

	if err := <-feedErr; err != nil {
		return nil, fmt.Errorf("batch feed: %w", err)
	}
	if err := batch.Wait(); err != nil {
		return nil, fmt.Errorf("batch wait: %w (stderr: %s)", err, stderrBuf.String())
	}
	return bodies, nil
}
