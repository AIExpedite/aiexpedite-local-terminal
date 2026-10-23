//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// whatsAppKeystoreCLITimeout bounds every credential-store call, so a locked
// keychain or a keyring prompt can never hang a link start or a cleanup.
const whatsAppKeystoreCLITimeout = 10 * time.Second

// runWhatsAppKeystoreCLI runs one credential-store command. The secret, when
// there is one, travels on STDIN only — never in argv, where any local user
// could read it from the process table.
var runWhatsAppKeystoreCLI = func(stdin string, name string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), whatsAppKeystoreCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			return "", -1, err
		}
	}
	return strings.TrimSpace(out.String()), code, nil
}

// whatsAppKeystoreCLIRunner is runWhatsAppKeystoreCLI's shape, so the
// secret-tool handling below can be tested with a scripted runner.
type whatsAppKeystoreCLIRunner func(stdin string, name string, args ...string) (string, int, error)

// readSecretToolKey reads a key through the Linux Secret Service.
//
// `secret-tool lookup` exits 1 with no output for a missing item, but ALSO
// for a service that failed (locked, unavailable) and for a locked item whose
// unlock prompt was dismissed. Reading any of those as "not found" makes the
// outbox discard its queued rows and mint a new key over the real one, so an
// empty answer is settled by secretToolItemAbsent. Only an absent item is
// errWhatsAppKeyNotFound; everything else is an error the caller retries. A
// run killed by the timeout exits -1.
func readSecretToolKey(run whatsAppKeystoreCLIRunner, account string) ([]byte, error) {
	out, code, err := run("", "secret-tool", "lookup", "service", whatsAppKeystoreService, "account", account)
	if err != nil {
		return nil, err
	}
	if code == 0 {
		return decodeWhatsAppKeystoreSecret(out)
	}
	if code != 1 || out != "" {
		return nil, fmt.Errorf("secret service read failed (exit %d)", code)
	}
	if err := secretToolItemAbsent(run, account); err != nil {
		return nil, err
	}
	return nil, errWhatsAppKeyNotFound
}

// clearSecretToolKey deletes a key through the Linux Secret Service.
// `secret-tool clear` exits 1 when nothing matched as well as when the
// service failed. Reporting an absent key as a failed delete would make every
// idempotent cleanup (a replayed unlink, a retried pending wipe) fail forever,
// so a refused clear is settled by secretToolItemAbsent: an absent item is
// errWhatsAppKeyNotFound, which deleteWhatsAppOutboxKey treats as deleted.
func clearSecretToolKey(run whatsAppKeystoreCLIRunner, account string) error {
	_, code, err := run("", "secret-tool", "clear", "service", whatsAppKeystoreService, "account", account)
	if err != nil {
		return err
	}
	if code == 0 {
		return nil
	}
	if code != 1 {
		return fmt.Errorf("secret service delete failed (exit %d)", code)
	}
	if err := secretToolItemAbsent(run, account); err != nil {
		return err
	}
	return errWhatsAppKeyNotFound
}

// secretToolItemAbsent settles an ambiguous lookup or clear with `secret-tool
// search`, which lists a matching item even while it is locked (it does not
// unlock, so it never prompts), exits 0 with no output when nothing matches,
// and 1 when the service fails. It returns nil only when the item is absent.
func secretToolItemAbsent(run whatsAppKeystoreCLIRunner, account string) error {
	found, code, err := run("", "secret-tool", "search", "service", whatsAppKeystoreService, "account", account)
	if err != nil {
		return err
	}
	switch {
	case code == 0 && found == "":
		return nil
	case code == 0:
		return errors.New("secret service item exists but is not accessible (locked?)")
	default:
		return fmt.Errorf("secret service search failed (exit %d)", code)
	}
}
