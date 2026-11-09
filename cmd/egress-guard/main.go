package main

import (
	"github.com/example/workspace-operator/internal/egress"
	"log"
	"os"
	"os/exec"
	"strings"
)

func values(key string) []string {
	out := []string{}
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
func main() {
	commands, err := egress.GuardCommands(values("EGRESS_PROTECTED_CIDRS"), values("EGRESS_DNS_SERVERS"))
	if err != nil {
		log.Fatal(err)
	}
	for _, c := range commands {
		// Init containers may retry in the same network namespace. Rebuild only
		// our chain, and do not insert duplicate OUTPUT jumps.
		if c[3] == "-I" {
			check := append([]string{}, c[1:3]...)
			check = append(check, "-C", c[4])
			check = append(check, c[6:]...)
			if exec.Command(c[0], check...).Run() == nil {
				continue
			}
		}
		b, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		if err != nil && c[3] == "-N" {
			// A missing command/permissions still fails on the flush; never ignore it.
			b, err = exec.Command(c[0], "-w", "5", "-F", "EGRESS_ENVOY").CombinedOutput()
		}
		if err != nil {
			log.Fatalf("guard command %v failed: %v: %s", c, err, b)
		}
	}
	log.Print("Envoy egress guard installed for UID 101")
}
