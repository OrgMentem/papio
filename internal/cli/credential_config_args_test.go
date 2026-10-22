package cli

import (
	"strings"
	"testing"
)

// TestCredentialArgumentCountRefusalWarnsAboutSecretsOnlyForSet: `set` is the
// one subcommand that takes a secret, so only its wrong-count refusal says
// credentials never go in the argument list. The others take a target or a
// reference; telling their callers about secrets is noise, so they get the usage
// line. Neither message may echo an argument, which might be the secret.
func TestCredentialArgumentCountRefusalWarnsAboutSecretsOnlyForSet(t *testing.T) {
	const secret = "synthetic-private-key-in-argv"
	for _, test := range []struct {
		name      string
		args      []string
		warns     bool
		wantUsage string
		agent     bool
	}{
		{name: "set with a trailing key", args: []string{"set", "sources.openaire", secret}, warns: true},
		{name: "status with two targets", args: []string{"status", "a", secret}, wantUsage: "status [TARGET]"},
		{name: "detach with no target", args: []string{"detach"}, wantUsage: "detach TARGET"},
		{name: "delete with two references", args: []string{"delete", "a", secret}, wantUsage: "delete REFERENCE"},
		{name: "bind with one argument", args: []string{"bind", "sources.openaire"}, wantUsage: "bind TARGET REFERENCE"},
		{name: "migrate with an argument", args: []string{"migrate", secret}, wantUsage: "migrate"},
		{name: "agent set with a trailing key", args: []string{"agent", "set", secret}, warns: true, agent: true},
		{name: "agent status with an argument", args: []string{"status", secret}, wantUsage: "status", agent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			var err error
			if test.agent {
				args := test.args
				if args[0] == "agent" {
					args = args[1:]
				}
				err = f.runAgent(args, "")
			} else {
				err = f.run(test.args, "")
			}
			if err == nil {
				t.Fatal("want an argument-count error, got nil")
			}
			if got := strings.Contains(err.Error(), "never as arguments"); got != test.warns {
				t.Fatalf("err = %q: warns about secrets in arguments = %t, want %t", err, got, test.warns)
			}
			if !test.warns && !strings.Contains(err.Error(), test.wantUsage) {
				t.Fatalf("err = %q, want it to show the usage %q", err, test.wantUsage)
			}
			if !isUsageError(err) {
				t.Fatalf("err = %v: --json must classify it as invalid_argument", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("err = %q echoes an argument", err)
			}
		})
	}
}
