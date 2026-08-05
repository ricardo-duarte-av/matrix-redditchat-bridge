package main

import (
	"fmt"
	"os"
	"regexp"

	"go.mau.fi/util/random"
	flag "maunium.net/go/mauflag"
	"maunium.net/go/mautrix/appservice"
)

const doublePuppetRegistrationPath = "doublepuppet-registration.yaml"

var generateDoublePuppetRegistration = flag.Make().
	LongKey("generate-doublepuppet-registration").
	Usage("Generate the second appservice registration used for double puppeting and quit.").
	Default("false").
	Bool()

// generateDoublePuppetRegistrationFile writes the second appservice registration. bridgev2
// already knows how to use an appservice token for double puppeting (double_puppet.secrets with
// an `as_token:` value), but mxmain only generates the bridge's own registration, so this fills
// the gap. Must be called after m.PreInit so the config is loaded.
func generateDoublePuppetRegistrationFile() {
	domain := m.Config.Homeserver.Domain
	if domain == "" || domain == "example.com" {
		fmt.Fprintln(os.Stderr, "Homeserver domain is not set in the config")
		os.Exit(20)
	}

	reg := appservice.CreateRegistration()
	reg.ID = m.Config.AppService.ID + "-doublepuppet"
	reg.SenderLocalpart = random.String(32)
	// No URL: this appservice never receives transactions, it only exists so the bridge can
	// masquerade as real users on this homeserver via the as_token.
	reg.URL = ""
	rateLimited := false
	reg.RateLimited = &rateLimited
	reg.Namespaces.UserIDs.Register(regexp.MustCompile(fmt.Sprintf(`^@.*:%s$`, regexp.QuoteMeta(domain))), false)

	if _, err := os.Stat(doublePuppetRegistrationPath); err == nil {
		fmt.Fprintf(os.Stderr, "%s already exists, remove it if you want to generate a new one\n", doublePuppetRegistrationPath)
		os.Exit(21)
	}
	if err := reg.Save(doublePuppetRegistrationPath); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to save double puppet registration:", err)
		os.Exit(21)
	}

	fmt.Printf(`Wrote %s

Install it on your homeserver alongside the main registration, then add this to config.yaml:

double_puppet:
    secrets:
        %s: as_token:%s
`, doublePuppetRegistrationPath, domain, reg.AppToken)
	os.Exit(0)
}
