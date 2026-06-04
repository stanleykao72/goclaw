package lineworks

import "testing"

func TestRoutePeer(t *testing.T) {
	c := &Channel{}
	c.groupChats.Store("grp-known", struct{}{})

	cases := []struct {
		name             string
		chatID           string
		metadata         map[string]string
		wantUser, wantCh string
	}{
		{
			name:     "group via metadata",
			chatID:   "g1",
			metadata: map[string]string{metaPeerKind: peerGroup, metaChannelID: "g1"},
			wantUser: "", wantCh: "g1",
		},
		{
			name:     "direct via metadata",
			chatID:   "u1",
			metadata: map[string]string{metaPeerKind: peerDirect, metaUserID: "u1"},
			wantUser: "u1", wantCh: "",
		},
		{
			name:     "group via remembered set (no metadata — agent reply path)",
			chatID:   "grp-known",
			metadata: nil,
			wantUser: "", wantCh: "grp-known",
		},
		{
			name:     "unknown chat without metadata falls back to 1:1 user",
			chatID:   "u2",
			metadata: nil,
			wantUser: "u2", wantCh: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, ch := c.routePeer(tc.chatID, tc.metadata)
			if u != tc.wantUser || ch != tc.wantCh {
				t.Errorf("routePeer(%q) = (user=%q, ch=%q); want (user=%q, ch=%q)",
					tc.chatID, u, ch, tc.wantUser, tc.wantCh)
			}
		})
	}
}
