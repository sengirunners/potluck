package taskproxy

import "strings"

// These suffixes identify public registration or shared tenant spaces, not a
// single operator's destinations. Their parents are equally broad wildcards.
var sharedSpaces = []string{
	"workers.dev", "pages.dev", "github.io", "githubusercontent.com", "cloudfront.net",
	"appspot.com", "herokuapp.com", "vercel.app", "netlify.app", "azurewebsites.net",
	"web.app", "firebaseapp.com", "ngrok.io", "trycloudflare.com", "r2.dev", "fly.dev",
	"ngrok-free.app", "ngrok.app", "ngrok-free.dev", "ngrok.dev", "onrender.com",
	"up.railway.app", "deno.dev", "gitlab.io", "glitch.me", "surge.sh", "run.app",
	"azurestaticapps.net", "azureedge.net", "loca.lt", "storage.googleapis.com",
	"amazonaws.com", "core.windows.net", "co.uk", "com.au", "co.jp", "co.in", "com.br",
}

func sameOrSubdomain(name, parent string) bool {
	return name == parent || strings.HasSuffix(name, "."+parent)
}

func validEntry(entry string) bool {
	name := strings.TrimPrefix(entry, "*.")
	last := name[strings.LastIndex(name, ".")+1:]
	if len(name) > 253 || !hostName.MatchString(name) || numericSuffix.MatchString(last) {
		return false
	}
	for _, service := range []string{"nip.io", "sslip.io"} {
		if sameOrSubdomain(name, service) {
			return false
		}
	}
	if !strings.HasPrefix(entry, "*.") {
		return true
	}
	// These providers have regional/service tenant spaces at many depths.
	for _, provider := range []string{"amazonaws.com", "core.windows.net"} {
		if sameOrSubdomain(name, provider) {
			return false
		}
	}
	for _, space := range sharedSpaces {
		if sameOrSubdomain(space, name) {
			return false
		}
	}
	return true
}
