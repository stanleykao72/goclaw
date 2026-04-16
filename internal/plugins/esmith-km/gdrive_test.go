package esmithkm

import (
	"reflect"
	"testing"
)

func TestExtractGdriveURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single view URL",
			in:   "https://drive.google.com/file/d/1-quJG9lECE2LvznAvzWQ8D7CDSwAob9H/view",
			want: []string{"https://drive.google.com/file/d/1-quJG9lECE2LvznAvzWQ8D7CDSwAob9H/view"},
		},
		{
			name: "view + usp=sharing",
			in:   "https://drive.google.com/file/d/1abc_DEF-ghi_JKL-mno_PQR/view?usp=sharing",
			want: []string{"https://drive.google.com/file/d/1abc_DEF-ghi_JKL-mno_PQR/view?usp=sharing"},
		},
		{
			name: "view + usp=drivesdk",
			in:   "https://drive.google.com/file/d/1-quJG9lECE2LvznAvzWQ8D7CDSwAob9H/view?usp=drivesdk",
			want: []string{"https://drive.google.com/file/d/1-quJG9lECE2LvznAvzWQ8D7CDSwAob9H/view?usp=drivesdk"},
		},
		{
			name: "edit URL",
			in:   "https://drive.google.com/file/d/1abc_DEF-ghi_JKL-mno_PQR-stu_VWXYZ/edit",
			want: []string{"https://drive.google.com/file/d/1abc_DEF-ghi_JKL-mno_PQR-stu_VWXYZ/edit"},
		},
		{
			name: "URL inside Chinese text",
			in:   "你看一下 https://drive.google.com/file/d/1abc_DEF-ghi_JKL-mno_PQR/view 這個會議錄音",
			want: []string{"https://drive.google.com/file/d/1abc_DEF-ghi_JKL-mno_PQR/view"},
		},
		{
			name: "http (not https)",
			in:   "http://drive.google.com/file/d/1abcDEFghiJKLmnoPQRstuVWX/view",
			want: []string{"http://drive.google.com/file/d/1abcDEFghiJKLmnoPQRstuVWX/view"},
		},
		{
			name: "two URLs in one message",
			in: "上午會議 https://drive.google.com/file/d/1AAAAAAAAAAAAAAAAAAAAAAAA/view " +
				"下午會議 https://drive.google.com/file/d/1BBBBBBBBBBBBBBBBBBBBBBBB/view",
			want: []string{
				"https://drive.google.com/file/d/1AAAAAAAAAAAAAAAAAAAAAAAA/view",
				"https://drive.google.com/file/d/1BBBBBBBBBBBBBBBBBBBBBBBB/view",
			},
		},
		{
			name: "URL ends at newline",
			in: "Top line https://drive.google.com/file/d/1XXXXXXXXXXXXXXXXXXXXXXXX/view\n" +
				"Other content",
			want: []string{"https://drive.google.com/file/d/1XXXXXXXXXXXXXXXXXXXXXXXX/view"},
		},
		{
			name: "non-GDrive URL",
			in:   "https://example.com/file/d/12345",
			want: nil,
		},
		{
			name: "GDrive folder URL (not file)",
			in:   "https://drive.google.com/drive/folders/1abc",
			want: nil,
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "no URL at all",
			in:   "hello world",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractGdriveURLs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractGdriveURLs(%q)\n  got:  %v\n  want: %v", tc.in, got, tc.want)
			}
		})
	}
}
