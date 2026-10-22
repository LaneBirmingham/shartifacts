package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/auth"
	"github.com/LaneBirmingham/shartifacts/internal/config"
)

const authUsage = `Usage: shartifacts auth [-data DIR] COMMAND

Sign-in from the server's own shell, for setting it up and for recovering a lost password.
With Docker: docker compose exec shartifacts shartifacts auth COMMAND
(add -T and pipe input in for set-password from a script).

Commands:
  status          whether sign-in is on, and how many sessions and tokens there are
  set-password    set the owner password, which turns sign-in on and signs every browser
                  out. Asks for it on a terminal, else reads its first line from standard input.
                  Agent tokens keep working: if someone else may know the old password, revoke
                  them too (revoke all)
  new-password    the same with a password made up for you, printed once
  off             remove the password: anyone who can reach the workspace can use it again
  token NAME      make an agent token and print it, once, on standard output
  tokens          list agent tokens
  revoke ID|all   revoke an agent token, or every one
  sign-out        sign every browser out

-data defaults to $SHARTIFACTS_DATA, else ./data. A running server picks changes up within
a second.
`

// authCommand runs `shartifacts auth` and returns the exit status.
func authCommand(args []string, stdin *os.File, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("shartifacts auth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, authUsage) }
	data := getenv(config.EnvName("data"))
	if data == "" {
		data = "./data"
	}
	fs.StringVar(&data, "data", data, "data directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cmd, rest := fs.Arg(0), fs.Args()
	if len(rest) > 0 {
		rest = rest[1:]
	}
	want := map[string]int{"status": 0, "set-password": 0, "new-password": 0, "off": 0, "token": 1, "tokens": 0, "revoke": 1, "sign-out": 0}
	n, known := want[cmd]
	if !known || len(rest) != n {
		if cmd != "" && cmd != "help" {
			fmt.Fprintf(stderr, "shartifacts auth: unknown command or wrong arguments: %s\n\n", strings.Join(fs.Args(), " "))
		}
		fmt.Fprint(stderr, authUsage)
		if cmd == "" || cmd == "help" {
			return 0
		}
		return 2
	}
	if _, err := os.Stat(data); err != nil {
		fmt.Fprintf(stderr, "shartifacts auth: data directory %s: %v (set -data or SHARTIFACTS_DATA)\n", data, err)
		return 1
	}
	st, err := auth.Open(data)
	if err != nil {
		fmt.Fprintf(stderr, "shartifacts auth: %v\n", err)
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "shartifacts auth %s: %v\n", cmd, err)
		return 1
	}
	switch cmd {
	case "status":
		s := st.Status()
		if !s.Enabled {
			fmt.Fprintln(stdout, "Sign-in is off: anyone who can reach the workspace can use it.")
		} else {
			fmt.Fprintf(stdout, "Sign-in is on (password set %s).\n", s.PasswordSetAt.Local().Format(time.DateTime))
		}
		fmt.Fprintf(stdout, "%d browser sessions, %d agent tokens.\n", s.Sessions, s.Tokens)
	case "set-password", "new-password":
		pw, made := "", cmd == "new-password"
		if made {
			pw = madeUpPassword()
		} else if pw, err = readPassword(stdin, stderr); err != nil {
			return fail(err)
		}
		if err = st.SetPassword(pw); err != nil {
			return fail(err)
		}
		if made {
			fmt.Fprintf(stdout, "Password: %s\n", pw)
			fmt.Fprintln(stdout, "Keep it in your password manager; you can change it under Sign-in in the workspace.")
		}
		fmt.Fprintln(stdout, "Sign-in is on, and every browser has been signed out. Agents need a token (shartifacts auth token NAME).")
		tokensLeft(stdout, st)
	case "off":
		if err = st.Disable(); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "Sign-in is off: anyone who can reach the workspace can use it.")
		tokensLeft(stdout, st)
	case "token":
		v, _, err := st.NewToken(rest[0])
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, v)
		fmt.Fprintln(stderr, "This is the only time the token is shown. Agents send it as: Authorization: Bearer <token>")
		if !st.Enabled() {
			fmt.Fprintln(stderr, "Sign-in is off, so agents do not need it until a password is set.")
		}
	case "tokens":
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSTARTS\tCREATED\tLAST USED")
		for _, t := range st.Tokens() {
			used := "never"
			if !t.LastUsedAt.IsZero() {
				used = t.LastUsedAt.Local().Format(time.DateTime)
			}
			fmt.Fprintf(w, "%s\t%s\t%s…\t%s\t%s\n", t.ID, t.Name, t.Hint, t.CreatedAt.Local().Format(time.DateTime), used)
		}
		w.Flush()
	case "revoke":
		if rest[0] == "all" {
			n, err := st.RevokeTokens()
			if err != nil {
				return fail(err)
			}
			fmt.Fprintf(stdout, "Revoked %d agent tokens.\n", n)
			break
		}
		if err = st.RevokeToken(rest[0]); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "Revoked.")
	case "sign-out":
		if err = st.EndSessions(); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "Every browser has been signed out.")
	}
	return 0
}

// tokensLeft says which agent tokens outlive a password change, since a lost or leaked
// password may mean someone else made some.
func tokensLeft(w io.Writer, st *auth.Store) {
	tokens := st.Tokens()
	if len(tokens) == 0 {
		return
	}
	names := make([]string, len(tokens))
	for i, t := range tokens {
		names[i] = t.Name
	}
	fmt.Fprintf(w, "%d agent tokens still work (%s). If someone else may have known the password, revoke them: shartifacts auth revoke all\n", len(tokens), strings.Join(names, ", "))
}

// readPassword asks for the new password twice on a terminal, with echo off, or takes the
// first line of standard input.
func readPassword(stdin *os.File, stderr io.Writer) (string, error) {
	fi, err := stdin.Stat()
	terminal := err == nil && fi.Mode()&os.ModeCharDevice != 0 && echo(stdin, false) == nil
	if terminal {
		defer echo(stdin, true)
		fmt.Fprint(stderr, "New password: ")
	}
	in := bufio.NewReader(io.LimitReader(stdin, 4*auth.MaxPassword))
	line := func() string {
		s, _ := in.ReadString('\n')
		return strings.TrimRight(s, "\r\n")
	}
	pw := line()
	if pw == "" {
		return "", errors.New("no password given: type one on a terminal, pipe one in, or use new-password")
	}
	if terminal {
		fmt.Fprint(stderr, "\nAgain: ")
		again := line()
		fmt.Fprintln(stderr)
		if again != pw {
			return "", errors.New("the two passwords differ")
		}
	}
	return pw, auth.ValidPassword(pw)
}

// echo turns a terminal's echo on or off with stty, which fails when f is not a terminal.
func echo(f *os.File, on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	c := exec.Command("stty", arg)
	c.Stdin = f
	return c.Run()
}

// madeUpPassword is 20 random characters (about 100 bits) in groups of four, from an
// alphabet without look-alikes.
func madeUpPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 20)
	rand.Read(b)
	var out strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return out.String()
}
