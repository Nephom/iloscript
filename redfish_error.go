package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type redfishErrorResponse struct {
	Error struct {
		Code         string `json:"code"`
		Message      string `json:"message"`
		ExtendedInfo []struct {
			MessageID string `json:"MessageId"`
			Message   string `json:"Message"`
		} `json:"@Message.ExtendedInfo"`
	} `json:"error"`
}

// formatRedfishError keeps HTTP failures useful without exposing the complete
// Redfish response, which can contain a large amount of implementation data.
func formatRedfishError(statusCode int, body []byte) string {
	var response redfishErrorResponse
	if err := json.Unmarshal(body, &response); err == nil {
		code := response.Error.Code
		message := response.Error.Message
		if len(response.Error.ExtendedInfo) > 0 {
			if code == "" {
				code = response.Error.ExtendedInfo[0].MessageID
			}
			if message == "" {
				message = response.Error.ExtendedInfo[0].Message
			}
		}
		if code != "" && message != "" {
			return fmt.Sprintf("HTTP %d: %s - %s", statusCode, code, message)
		}
		if code != "" {
			return fmt.Sprintf("HTTP %d: %s", statusCode, code)
		}
		if message != "" {
			return fmt.Sprintf("HTTP %d: %s", statusCode, message)
		}
	}

	status := http.StatusText(statusCode)
	if status == "" {
		status = "request failed"
	}
	return fmt.Sprintf("HTTP %d: %s", statusCode, strings.TrimSpace(status))
}
