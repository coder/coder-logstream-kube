package main

import (
	"strings"
	"testing"
)

func TestRootCommand_URLValidation(t *testing.T) {
	tests := []struct {
		name      string
		coderURL  string
		wantError string
	}{
		{
			name:      "missing scheme",
			coderURL:  "coder.example.com",
			wantError: "CODER_URL must include http:// or https:// scheme",
		},
		{
			name:      "empty scheme with path",
			coderURL:  "//coder.example.com",
			wantError: "CODER_URL must include http:// or https:// scheme",
		},
		{
			name:      "ftp scheme",
			coderURL:  "ftp://coder.example.com",
			wantError: "CODER_URL must include http:// or https:// scheme",
		},
		{
			name:      "empty URL",
			coderURL:  "",
			wantError: "--coder-url is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := root()
			cmd.SetArgs([]string{"--coder-url", tt.coderURL})

			err := cmd.Execute()

			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantError)
			} else if !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("expected error containing %q, got %q", tt.wantError, err.Error())
			}
		})
	}
}
