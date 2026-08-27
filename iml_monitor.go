package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// ctrlF is the raw byte value of Ctrl+F in terminal input (ASCII 0x06 / ACK).
const ctrlF = 0x06

// formatIMLEventTime decides how the "time this event occurred" should be
// displayed: prefer the Created timestamp returned by iLO (if present and
// parseable), converting it to the Asia/Taipei timezone to match the existing
// Event Log convention; if iLO doesn't return a usable Created timestamp,
// fall back to the local system time at the moment iloscript detected this
// new event.
func formatIMLEventTime(entry imlEvent) string {
	if entry.Created != "" {
		if ts, err := time.Parse(time.RFC3339, entry.Created); err == nil {
			loc, locErr := time.LoadLocation("Asia/Taipei")
			if locErr != nil {
				loc = time.Local
			}
			return ts.In(loc).Format("2006-01-02 15:04:05")
		}
	}

	return time.Now().Format("2006-01-02 15:04:05")
}

// clearScreen clears the terminal screen and moves the cursor back to the
// top-left corner, so the top live list can be refreshed each time like the
// `watch` command, while the continuously accumulating new-event log below
// is also reprinted in full.
func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

// watchForFlushKey continuously reads stdin byte-by-byte in the background,
// and once it detects Ctrl+F (0x06) it notifies the main loop via flush to
// clear the new-event log below. This is a blocking read; it is naturally
// reclaimed when the program exits (process termination), so no extra stop
// mechanism is needed.
func watchForFlushKey(flush chan<- struct{}) {
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		if n > 0 && buf[0] == ctrlF {
			select {
			case flush <- struct{}{}:
			default:
			}
		}
	}
}

func (c *ILOClient) MonitorIML(ctx context.Context, count int, matchText, severity string) error {
	if count < 1 {
		return fmt.Errorf("IML fetch count must be positive")
	}

	separator := strings.Repeat("=", 47)
	resetColor := "\033[0m"

	// Try to switch the terminal into cbreak mode so the program can detect
	// Ctrl+F immediately (without needing Enter). If the current stdin isn't
	// a real terminal (e.g. redirected, or running in a non-interactive
	// environment), enabling it will fail; in that case, gracefully disable
	// the "clear the log below" feature without affecting any other
	// monitoring behavior.
	flushHint := ""
	flushRequests := make(chan struct{}, 1)
	stdinFd := int(os.Stdin.Fd())
	if orig, err := enableCbreakMode(stdinFd); err == nil {
		defer restoreTerminal(stdinFd, orig)
		go watchForFlushKey(flushRequests)
		flushHint = "(Press Ctrl+F to clear the new-event log below; the live list above is unaffected. Press Ctrl+C to stop monitoring)"
	}

	fmt.Println("Monitoring IML every 5 seconds. Press Ctrl+C to stop.")
	if flushHint != "" {
		fmt.Println(flushHint)
	}

	// seen tracks the IML entries seen so far (keyed by the entry's actual
	// number on iLO), used to determine which entries are "new this round".
	// newEventLines is the continuously accumulating bottom-half log, which
	// only changes when the user presses Ctrl+F or the IML log is detected
	// as cleared.
	seen := make(map[int]bool)
	var newEventLines []string
	baselineEstablished := false
	prevTotalCount := -1
	reconnectAttempts := 0
	var topLines []string

	render := func() {
		clearScreen()
		for _, line := range topLines {
			fmt.Println(line)
		}
		fmt.Println(separator)
		if flushHint != "" {
			fmt.Println(flushHint)
		}
		for _, line := range newEventLines {
			fmt.Println(line)
		}
	}

	doFetch := func() {
		members, totalCount, truncatedAtBoundary, err := c.fetchIMLEntries(count, matchText, severity)
		fetchTimeStr := time.Now().Format("2006-01-02 15:04:05")

		if err != nil {
			// Fetch failed (likely because iLO is rebooting or the connection
			// was lost): show the error and reconnect status in the live
			// section above, leaving the accumulated new-event log below
			// unchanged.
			reconnectAttempts++
			lines := []string{fmt.Sprintf("IML monitoring encountered an error (time: %s): %v", fetchTimeStr, err)}
			if isSessionError(err) {
				lines = append(lines, fmt.Sprintf("Reconnecting to iLO session... (attempt %d)", reconnectAttempts))
				if reconnectErr := c.Reconnect(); reconnectErr != nil {
					lines = append(lines, fmt.Sprintf("Reconnect failed: %v", reconnectErr))
				}
			}
			topLines = lines
			return
		}
		reconnectAttempts = 0

		header := fmt.Sprintf("Recent IML events (fetched at: %s):", fetchTimeStr)
		groups := groupIMLEvents(members, truncatedAtBoundary)
		groupLines := formatIMLGroupLines(groups)

		// Under normal operation, IML entries only ever accumulate; they are
		// only cleared entirely via a ClearLog action. So if this round's
		// total count is lower than the previous round's, treat it as
		// having been cleared.
		cleared := prevTotalCount >= 0 && totalCount < prevTotalCount

		if cleared {
			topLines = append([]string{header, ">>> IML log clear detected <<<"}, groupLines...)

			// After clearing, iLO restarts numbering from 1, so the old
			// seen set is no longer meaningful. Rebuild the baseline from
			// the entries just fetched, to avoid mistaking reused old
			// numbers as "already seen" and missing genuinely new events;
			// the log below is left unchanged as required.
			seen = make(map[int]bool)
			for _, entry := range members {
				seen[entry.ID] = true
			}
		} else {
			topLines = append([]string{header}, groupLines...)

			if !baselineEstablished {
				// The first successful fetch is only used to establish the
				// baseline and isn't treated as "new events", to avoid
				// treating everything that already existed at startup as new.
				for _, entry := range members {
					seen[entry.ID] = true
				}
				baselineEstablished = true
			} else {
				for _, entry := range members {
					if seen[entry.ID] {
						continue
					}
					seen[entry.ID] = true

					colorCode := imlSeverityColor(entry.Severity)
					line := fmt.Sprintf("%s- %s %s: %s%s", colorCode, formatIMLEventTime(entry), entry.Severity, entry.Message, resetColor)
					newEventLines = append(newEventLines, line)
				}
			}
		}

		prevTotalCount = totalCount
	}

	for {
		doFetch()
		render()

		timer := time.NewTimer(monitorInterval)
		waiting := true
		for waiting {
			select {
			case <-ctx.Done():
				timer.Stop()
				fmt.Println("IML monitoring stopped.")
				return nil
			case <-flushRequests:
				// Ctrl+F: only clear the new-event log below; the live list
				// above is left unchanged.
				newEventLines = nil
				render()
			case <-timer.C:
				waiting = false
			}
		}
	}
}
