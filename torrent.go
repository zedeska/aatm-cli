package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// TorrentOptions controls how the torrent file is created.
type TorrentOptions struct {
	// Announce URLs (first = primary, rest go into announce-list)
	Trackers []string
	// If non-empty, sets the "source" field in the info dict (e.g. "lacale")
	Source string
	// Output path for the .torrent file; defaults to <inputName>.torrent
	OutputPath string
}

// CreateTorrent creates a private .torrent file for the given file or directory.
// Returns the path to the created .torrent and the info-hash (hex).
func CreateTorrent(inputPath string, opts TorrentOptions) (torrentPath string, infoHash string, err error) {
	stat, err := os.Stat(inputPath)
	if err != nil {
		return "", "", fmt.Errorf("cannot stat input: %w", err)
	}

	// Collect files to include
	type fileEntry struct {
		absPath  string
		relParts []string
		size     int64
	}

	var files []fileEntry
	var totalSize int64

	if stat.IsDir() {
		err = filepath.Walk(inputPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(inputPath, path)
			if err != nil {
				return err
			}
			parts := splitPath(rel)
			files = append(files, fileEntry{absPath: path, relParts: parts, size: info.Size()})
			totalSize += info.Size()
			return nil
		})
		if err != nil {
			return "", "", fmt.Errorf("walking directory: %w", err)
		}
	} else {
		files = []fileEntry{
			{absPath: inputPath, relParts: []string{stat.Name()}, size: stat.Size()},
		}
		totalSize = stat.Size()
	}

	if len(files) == 0 {
		return "", "", fmt.Errorf("no files found in %s", inputPath)
	}

	pieceLen := choosePieceLength(totalSize)

	// Compute pieces by reading all files in order
	pieces, err := computePieces(files, func(fe fileEntry) (string, int64) {
		return fe.absPath, fe.size
	}, pieceLen)
	if err != nil {
		return "", "", fmt.Errorf("computing pieces: %w", err)
	}

	// Build info dict
	info := map[string]interface{}{}
	info["name"] = stat.Name()
	info["piece length"] = pieceLen
	info["pieces"] = []byte(pieces)
	info["private"] = 1

	if opts.Source != "" {
		info["source"] = opts.Source
	}

	if stat.IsDir() {
		fileList := make([]interface{}, len(files))
		for i, fe := range files {
			parts := make([]interface{}, len(fe.relParts))
			for j, p := range fe.relParts {
				parts[j] = p
			}
			fileList[i] = map[string]interface{}{
				"length": fe.size,
				"path":   parts,
			}
		}
		info["files"] = fileList
	} else {
		info["length"] = files[0].size
	}

	// Bencode info dict to compute infohash
	encodedInfo, err := bencode(info)
	if err != nil {
		return "", "", fmt.Errorf("bencoding info dict: %w", err)
	}

	h := sha1.Sum(encodedInfo)
	infoHash = hex.EncodeToString(h[:])

	// Build announce-list: each tracker in a separate tier for max compatibility
	announceList := make([]interface{}, len(opts.Trackers))
	for i, t := range opts.Trackers {
		announceList[i] = []interface{}{t}
	}

	metainfo := map[string]interface{}{
		"info":          info,
		"announce-list": announceList,
	}
	if len(opts.Trackers) > 0 {
		metainfo["announce"] = opts.Trackers[0]
	}

	encoded, err := bencode(metainfo)
	if err != nil {
		return "", "", fmt.Errorf("bencoding metainfo: %w", err)
	}

	// Determine output path
	torrentPath = opts.OutputPath
	if torrentPath == "" {
		dir := filepath.Dir(inputPath)
		torrentPath = filepath.Join(dir, stat.Name()+".torrent")
	}

	if err := os.WriteFile(torrentPath, encoded, 0o644); err != nil {
		return "", "", fmt.Errorf("writing torrent file: %w", err)
	}

	return torrentPath, infoHash, nil
}

// choosePieceLength returns an appropriate piece length for the total file size.
func choosePieceLength(totalSize int64) int64 {
	const (
		kb = 1024
		mb = 1024 * kb
	)
	switch {
	case totalSize < 64*mb:
		return 256 * kb
	case totalSize < 512*mb:
		return 512 * kb
	case totalSize < 1024*mb:
		return 1 * mb
	case totalSize < 4*1024*mb:
		return 2 * mb
	default:
		return 4 * mb
	}
}

// computePieces reads all files sequentially and builds the SHA1 piece string.
func computePieces[T any](files []T, getInfo func(T) (string, int64), pieceLen int64) (string, error) {
	var pieceBuf bytes.Buffer
	var buf []byte = make([]byte, pieceLen)
	var pos int64

	var current *os.File
	fileIdx := 0
	var filePos int64

	openNext := func() error {
		if current != nil {
			current.Close()
			current = nil
		}
		if fileIdx >= len(files) {
			return nil
		}
		path, _ := getInfo(files[fileIdx])
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening %s: %w", path, err)
		}
		current = f
		filePos = 0
		fileIdx++
		return nil
	}

	if err := openNext(); err != nil {
		return "", err
	}

	for {
		remaining := pieceLen - (pos % pieceLen)
		toRead := remaining
		chunk := buf[:toRead]
		var n int64
		for n < toRead && fileIdx <= len(files) {
			if current == nil {
				break
			}
			nr, err := current.Read(chunk[n:])
			n += int64(nr)
			filePos += int64(nr)
			if err == io.EOF {
				// Try next file
				_, size := getInfo(files[fileIdx-1])
				_ = size
				if err2 := openNext(); err2 != nil {
					return "", err2
				}
				if current == nil {
					break
				}
			} else if err != nil {
				return "", fmt.Errorf("reading: %w", err)
			}
		}

		if n == 0 {
			break
		}

		h := sha1.Sum(chunk[:n])
		pieceBuf.Write(h[:])
		pos += n
	}

	if current != nil {
		current.Close()
	}

	return pieceBuf.String(), nil
}

// splitPath splits a filepath into individual directory/file components.
func splitPath(p string) []string {
	var parts []string
	for {
		dir, file := filepath.Split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" || dir == "/" || dir == p {
			break
		}
		p = filepath.Clean(dir)
	}
	return parts
}

// ── bencode encoder ─────────────────────────────────────────────────────────

func bencode(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if err := bencodeValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func bencodeValue(w *bytes.Buffer, v interface{}) error {
	switch val := v.(type) {
	case string:
		w.WriteString(strconv.Itoa(len(val)))
		w.WriteByte(':')
		w.WriteString(val)
	case []byte:
		w.WriteString(strconv.Itoa(len(val)))
		w.WriteByte(':')
		w.Write(val)
	case int:
		w.WriteByte('i')
		w.WriteString(strconv.Itoa(val))
		w.WriteByte('e')
	case int64:
		w.WriteByte('i')
		w.WriteString(strconv.FormatInt(val, 10))
		w.WriteByte('e')
	case []interface{}:
		w.WriteByte('l')
		for _, item := range val {
			if err := bencodeValue(w, item); err != nil {
				return err
			}
		}
		w.WriteByte('e')
	case map[string]interface{}:
		w.WriteByte('d')
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys) // bencode dicts MUST have sorted keys
		for _, k := range keys {
			if err := bencodeValue(w, k); err != nil {
				return err
			}
			if err := bencodeValue(w, val[k]); err != nil {
				return err
			}
		}
		w.WriteByte('e')
	default:
		return fmt.Errorf("unsupported bencode type: %T", v)
	}
	return nil
}
