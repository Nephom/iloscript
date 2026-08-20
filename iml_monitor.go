package main

import (
	"context"
	"fmt"
)

func (c *ILOClient) MonitorIML(ctx context.Context, count int, matchText, severity string) error {
	if count < 1 {
		return fmt.Errorf("IML fetch count must be positive")
	}
	fmt.Println("Monitoring IML every 5 seconds. Press Ctrl+C to stop.")
	for {
		if err := c.FetchIMLEvents(count, matchText, severity); err != nil {
			fmt.Printf("IML fetch failed: %v\n", err)
			if isSessionError(err) {
				fmt.Println("Reconnecting to iLO session...")
				if reconnectErr := c.Reconnect(); reconnectErr != nil {
					fmt.Printf("Reconnect failed: %v\n", reconnectErr)
				}
			}
		}
		if err := sleepWithContext(ctx, monitorInterval); err != nil {
			fmt.Println("IML monitoring stopped.")
			return nil
		}
	}
}
