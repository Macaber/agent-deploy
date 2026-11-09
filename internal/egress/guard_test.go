package egress

import (
	"strings"
	"testing"
)

func TestGuardBlocksProtectedBeforeTCP(t *testing.T) {
	if _, err := GuardCommands(nil, []string{"10.0.0.10"}); err == nil {
		t.Fatal("missing inventory accepted")
	}
	if _, err := GuardCommands([]string{"bad"}, []string{"10.0.0.10"}); err == nil {
		t.Fatal("bad inventory accepted")
	}
	cmds, err := GuardCommands([]string{"10.0.0.0/8"}, []string{"10.96.0.10"})
	if err != nil {
		t.Fatal(err)
	}
	text := []string{}
	for _, c := range cmds {
		text = append(text, strings.Join(c, " "))
	}
	s := strings.Join(text, "\n")
	for _, v := range []string{"--dst-type LOCAL -j REJECT", "-d 169.254.0.0/16 -j REJECT", "--uid-owner 101", "ip6tables"} {
		if !strings.Contains(s, v) {
			t.Fatal("missing protection", v)
		}
	}
	if strings.Index(s, "-d 10.0.0.0/8 -j REJECT") > strings.Index(s, "-p tcp -j RETURN") {
		t.Fatal("TCP allowed before protection")
	}
}
