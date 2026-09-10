package main

import "testing"

func TestFormatRedfishError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{
			name:       "standard error",
			statusCode: 404,
			body:       `{"error":{"code":"Base.1.8.ResourceMissingAtURI","message":"The resource was not found.","@Message.ExtendedInfo":[{"MessageId":"Base.1.8.ResourceMissingAtURI","Message":"The resource was not found.","Resolution":"Retry the request."}]}}`,
			want:       "HTTP 404: Base.1.8.ResourceMissingAtURI - The resource was not found.",
		},
		{
			name:       "extended info fallback",
			statusCode: 400,
			body:       `{"error":{"@Message.ExtendedInfo":[{"MessageId":"Base.1.8.GeneralError","Message":"The request is invalid."}]}}`,
			want:       "HTTP 400: Base.1.8.GeneralError - The request is invalid.",
		},
		{
			name:       "non JSON response",
			statusCode: 502,
			body:       `{"unexpected":"large response"}`,
			want:       "HTTP 502: Bad Gateway",
		},
		{
			name:       "unknown status",
			statusCode: 799,
			body:       "not exposed",
			want:       "HTTP 799: request failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatRedfishError(test.statusCode, []byte(test.body)); got != test.want {
				t.Fatalf("formatRedfishError() = %q, want %q", got, test.want)
			}
		})
	}
}
