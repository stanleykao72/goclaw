package nlmdoc

import "testing"

func TestDocName(t *testing.T) {
	cases := []struct {
		name        string
		scopeKind   string
		scopeID     string
		displayName string
		want        string
	}{
		{
			name:      "shared has no id and no display",
			scopeKind: "shared", scopeID: "", displayName: "",
			want: "shared",
		},
		{
			name:      "user id-only when no display name",
			scopeKind: "user", scopeID: "123456789abc", displayName: "",
			want: "user-12345678",
		},
		{
			name:      "user with Chinese display name",
			scopeKind: "user", scopeID: "123456789abc", displayName: "王小明",
			want: "user-12345678-王小明",
		},
		{
			name:      "agent uses key as id",
			scopeKind: "agent", scopeID: "e-smith-hub", displayName: "",
			want: "agent-e-smith",
		},
		{
			name:      "group chatId truncated to 8",
			scopeKind: "group", scopeID: "chat-abcdefghij", displayName: "工地群",
			want: "group-chat-abc-工地群",
		},
		{
			name:      "short id is not truncated",
			scopeKind: "user", scopeID: "ab", displayName: "",
			want: "user-ab",
		},
		{
			name:      "unsafe chars in display sanitized",
			scopeKind: "user", scopeID: "12345678", displayName: "a/b:c*d",
			want: "user-12345678-a_b_c_d",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DocName(tc.scopeKind, tc.scopeID, tc.displayName)
			if got != tc.want {
				t.Fatalf("DocName(%q,%q,%q) = %q, want %q",
					tc.scopeKind, tc.scopeID, tc.displayName, got, tc.want)
			}
		})
	}
}

func TestSanitizePathSegment(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"clean", "clean"},
		{"a/b\\c", "a_b_c"},
		{`x:y*z?"<>|`, "x_y_z"},
		{"  spaced  ", "spaced"},
		{"王小明", "王小明"},
		{"multi___under", "multi_under"},
		{"___leading_trailing___", "leading_trailing"},
		{"////", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := SanitizePathSegment(tc.in)
		if got != tc.want {
			t.Fatalf("SanitizePathSegment(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizePathSegment_DropsControlChars(t *testing.T) {
	if got := SanitizePathSegment("a\x00b\x07c"); got != "abc" {
		t.Fatalf("got %q, want abc", got)
	}
}
