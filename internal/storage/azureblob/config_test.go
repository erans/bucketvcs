package azureblob

import "testing"

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ok with account+container", Config{Account: "acct", Container: "c"}, ""},
		{"ok with conn string", Config{ConnectionString: "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=k;BlobEndpoint=http://x;", Container: "c"}, ""},
		{"missing container", Config{Account: "acct"}, "container is required"},
		{"missing account and conn string", Config{Container: "c"}, "account or connection string"},
		{"bad prefix", Config{Account: "a", Container: "c", Prefix: "//bad"}, "invalid prefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: want nil, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate: want %q, got nil", tc.wantErr)
			case tc.wantErr != "":
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate: want %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
