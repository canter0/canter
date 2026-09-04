package main

import "testing"

func TestCookieSecurityFollowsPublicURL(t *testing.T) {
	tests := []struct {
		name       string
		publicURL  string
		configured string
		want       bool
		wantErr    bool
	}{
		{name: "https forces secure", publicURL: "https://canter.dev", configured: "false", want: true},
		{name: "local http defaults insecure", publicURL: "http://localhost:3001", want: false},
		{name: "explicit local secure", publicURL: "http://localhost:3001", configured: "true", want: true},
		{name: "reject relative", publicURL: "/canter", wantErr: true},
		{name: "reject unsupported scheme", publicURL: "ftp://canter.dev", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := cookieSecurity(test.publicURL, test.configured)
			if (err != nil) != test.wantErr {
				t.Fatalf("cookieSecurity error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("cookieSecurity = %v, want %v", got, test.want)
			}
		})
	}
}
