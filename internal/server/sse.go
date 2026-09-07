package server

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// streamSSE relays r as Server-Sent Events, one "data:" frame per line of input, flushing after
// each event so a follower sees output as it arrives rather than buffered.
func streamSSE(w http.ResponseWriter, r io.Reader) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl, canFlush := w.(http.Flusher)
	if canFlush {
		fl.Flush()
	}

	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\n")
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", line); werr != nil {
				return werr
			}
			if canFlush {
				fl.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				fmt.Fprint(w, "event: end\ndata: \n\n")
				if canFlush {
					fl.Flush()
				}
				return nil
			}
			return err
		}
	}
}

// streamChunked relays r as plain chunked text, flushing after every read so a follower sees
// output as it arrives.
func streamChunked(w http.ResponseWriter, r io.Reader) error {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	// Flush right away so the client sees response headers immediately, even if r's first Read
	// blocks for a while (e.g. a follow=true stream on a job that hasn't produced output yet) —
	// without this, net/http buffers the header until the first Write/Flush, so a slow-to-produce
	// stream looks like a hung connection with no response at all.
	if fl != nil {
		fl.Flush()
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
