package compose

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Magic variables SERVICE_<TYPE>_<ID> (compose-spec §4): platform-generated,
// persistent across redeployments, shared by every service of the stack.
// This file is the PURE part — parsing, scanning, generation. Persistence in
// environment_variables and FQDN/URL resolution belong to the engine, which
// has the database and the domains.

// MagicType is a supported <TYPE> (§4.2).
type MagicType string

// Magic value placeholders substituted at deploy time (compose-spec).
const (
	MagicFQDN                MagicType = "FQDN"
	MagicURL                 MagicType = "URL"
	MagicUser                MagicType = "USER"
	MagicPassword            MagicType = "PASSWORD"
	MagicPasswordWithSymbols MagicType = "PASSWORDWITHSYMBOLS"
	MagicBase64              MagicType = "BASE64"
	MagicRealBase64          MagicType = "REALBASE64"
	MagicHex                 MagicType = "HEX"
)

// MagicRef is one parsed SERVICE_* reference.
type MagicRef struct {
	// Name is the full variable name as referenced.
	Name string
	Type MagicType
	// ID is the component identifier ([A-Z0-9_]+). The same ID means the
	// same value across the whole stack (§4.1).
	ID string
	// Length is the generation length (§4.2); 0 for FQDN/URL.
	Length int
	// Port is the internal port for FQDN/URL variants (§4.2); 0 if absent.
	Port int
	// Credential marks the types stored is_secret = true (§4.3).
	Credential bool
}

// magicName matches any SERVICE_* token in a compose file.
var magicName = regexp.MustCompile(`\bSERVICE_[A-Z0-9_]+\b`)

// NormalizeComponentID converts a compose service name to its magic <ID>
// (§4.1): uppercase, non-alphanumerics replaced by underscores.
func NormalizeComponentID(service string) string {
	up := strings.ToUpper(service)
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, up)
}

// lengths allowed per type; index 0 is the default when no length is given.
var magicLengths = map[MagicType][]int{
	MagicUser:                {16},
	MagicPassword:            {32, 64},
	MagicPasswordWithSymbols: {32, 64},
	MagicBase64:              {32, 64, 128},
	MagicRealBase64:          {32, 64, 128},
	MagicHex:                 {32, 64, 128},
}

// ParseMagicName parses one SERVICE_* variable name. components are the
// normalized IDs of the stack's services — needed to split the ID from an
// optional trailing port on FQDN/URL variants (§4.1–4.2).
func ParseMagicName(name string, components map[string]bool) (MagicRef, *Finding) {
	rest, ok := strings.CutPrefix(name, "SERVICE_")
	if !ok || rest == "" {
		return MagicRef{}, &Finding{Code: CodeMagicVariableInvalidType, Severity: Error, Message: fmt.Sprintf("%s is not a magic variable", name)}
	}

	ref := MagicRef{Name: name}
	for _, t := range []MagicType{MagicPasswordWithSymbols, MagicRealBase64, MagicPassword, MagicBase64, MagicFQDN, MagicURL, MagicUser, MagicHex} {
		if after, ok := strings.CutPrefix(rest, string(t)+"_"); ok {
			ref.Type = t
			rest = after
			break
		}
	}
	if ref.Type == "" {
		return MagicRef{}, &Finding{Code: CodeMagicVariableInvalidType, Severity: Error, Message: fmt.Sprintf("%s: unknown magic variable type", name)}
	}

	switch ref.Type {
	case MagicFQDN, MagicURL:
		// SERVICE_FQDN_<ID> or SERVICE_FQDN_<ID>_<PORT>. The ID itself may end
		// with digits, so an existing component always wins over a port read.
		ref.ID = rest
		if !components[ref.ID] {
			if i := strings.LastIndex(rest, "_"); i > 0 {
				if port, err := strconv.Atoi(rest[i+1:]); err == nil && components[rest[:i]] {
					ref.ID, ref.Port = rest[:i], port
				}
			}
		}
		if !components[ref.ID] {
			return MagicRef{}, &Finding{Code: CodeMagicVariableUnknownComp, Severity: Error, Message: fmt.Sprintf("%s: no component matches %q", name, ref.ID)}
		}
		return ref, nil
	default:
		allowed := magicLengths[ref.Type]
		ref.Length = allowed[0]
		if i := strings.Index(rest, "_"); i > 0 {
			if n, err := strconv.Atoi(rest[:i]); err == nil {
				valid := false
				for _, l := range allowed {
					if n == l {
						valid = true
					}
				}
				if !valid {
					return MagicRef{}, &Finding{Code: CodeMagicVariableInvalidType, Severity: Error, Message: fmt.Sprintf("%s: length %d is not one of %v", name, n, allowed)}
				}
				ref.Length = n
				rest = rest[i+1:]
			}
		}
		if rest == "" || !regexp.MustCompile(`^[A-Z0-9_]+$`).MatchString(rest) {
			return MagicRef{}, &Finding{Code: CodeMagicVariableInvalidType, Severity: Error, Message: fmt.Sprintf("%s: missing or invalid identifier", name)}
		}
		ref.ID = rest
		ref.Credential = true
		return ref, nil
	}
}

// ScanMagicReferences finds every SERVICE_* reference of a compose file and
// parses them against the stack's services. Each distinct name is returned
// once, sorted — determinism again (INV-011).
func ScanMagicReferences(content string, services []string) ([]MagicRef, []Finding) {
	components := map[string]bool{}
	for _, s := range services {
		components[NormalizeComponentID(s)] = true
	}

	seen := map[string]bool{}
	var refs []MagicRef
	var fs []Finding
	for _, name := range magicName.FindAllString(content, -1) {
		if seen[name] {
			continue
		}
		seen[name] = true
		ref, finding := ParseMagicName(name, components)
		if finding != nil {
			fs = append(fs, *finding)
			continue
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, fs
}

// Alphabets of §4.2.
const (
	alphaLower       = "abcdefghijklmnopqrstuvwxyz"
	alphaLowerDigits = alphaLower + "0123456789"
	alphaMixedDigits = "ABCDEFGHIJKLMNOPQRSTUVWXYZ" + alphaLowerDigits
	passwordSymbols  = "!@#$%^&*()-_=+[]{}<>~"
	alphaWithSymbols = alphaMixedDigits + passwordSymbols
)

// GenerateMagicValue produces the value of a credential-type reference with
// a CSPRNG (§4.2). FQDN/URL types are resolved from domains by the engine,
// never generated here.
func GenerateMagicValue(ref MagicRef) (string, error) {
	switch ref.Type {
	case MagicUser:
		first, err := randomFrom(alphaLower, 1)
		if err != nil {
			return "", err
		}
		restChars, err := randomFrom(alphaLowerDigits, ref.Length-1)
		if err != nil {
			return "", err
		}
		return first + restChars, nil
	case MagicPassword, MagicBase64:
		return randomFrom(alphaMixedDigits, ref.Length)
	case MagicPasswordWithSymbols:
		return randomFrom(alphaWithSymbols, ref.Length)
	case MagicRealBase64:
		raw := make([]byte, ref.Length)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(raw), nil
	case MagicHex:
		raw := make([]byte, ref.Length/2)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		return hex.EncodeToString(raw), nil
	default:
		return "", fmt.Errorf("magic type %s is not generated", ref.Type)
	}
}

// randomFrom draws n characters uniformly from an alphabet — modulo-free,
// so no character is likelier than another.
func randomFrom(alphabet string, n int) (string, error) {
	out := make([]byte, n)
	size := big.NewInt(int64(len(alphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, size)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}
