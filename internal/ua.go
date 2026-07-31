package internal

import (
	"fmt"
	"net/http"
	"strings"
)

type UAProfile struct {
	Name        string `json:"name"`
	UserAgent   string `json:"user_agent"`
	Client      string `json:"client"`
	Version     string `json:"version"`
	Passthrough bool   `json:"-"`
}

var uaProfiles = map[string]UAProfile{
	"infuse": {Name: "Infuse", UserAgent: "Infuse/7.8.1", Client: "Infuse", Version: "7.8.1"},
	"web":    {Name: "Web", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Emby Theater", Client: "Emby Web", Version: "4.9.0.42"},
	"client": {Name: "Client", UserAgent: "Emby-Theater/4.7.0", Client: "Emby Theater", Version: "4.7.0"},
}

const (
	customUAMode          = "custom"
	passthroughUAMode     = "passthrough"
	maxCustomUserAgentLen = 1024
	maxCustomClientLen    = 128
	maxCustomVersionLen   = 64
)

func getUAProfile(mode string) UAProfile {
	if p, ok := uaProfiles[strings.ToLower(mode)]; ok {
		return p
	}
	return uaProfiles["infuse"]
}

func validateCustomUAValue(field, value string, maxLen int, allowQuotes bool) error {
	if value == "" {
		return fmt.Errorf("custom %s is required", field)
	}
	if len(value) > maxLen {
		return fmt.Errorf("custom %s must be at most %d bytes", field, maxLen)
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("custom %s must contain printable ASCII characters only", field)
		}
		if !allowQuotes && (r == '"' || r == '\\') {
			return fmt.Errorf("custom %s must not contain quotes or backslashes", field)
		}
	}
	return nil
}

func normalizeUAConfig(mode, userAgent, client, version string) (string, string, string, string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == passthroughUAMode {
		return mode, "", "", "", nil
	}
	if mode != customUAMode {
		if _, ok := uaProfiles[mode]; !ok {
			return "", "", "", "", fmt.Errorf("unknown ua_mode")
		}
		return mode, "", "", "", nil
	}

	userAgent = strings.TrimSpace(userAgent)
	client = strings.TrimSpace(client)
	version = strings.TrimSpace(version)
	if err := validateCustomUAValue("user_agent", userAgent, maxCustomUserAgentLen, true); err != nil {
		return "", "", "", "", err
	}
	if err := validateCustomUAValue("client", client, maxCustomClientLen, false); err != nil {
		return "", "", "", "", err
	}
	if err := validateCustomUAValue("version", version, maxCustomVersionLen, false); err != nil {
		return "", "", "", "", err
	}
	return mode, userAgent, client, version, nil
}

func resolveSiteUAProfile(site Site) (UAProfile, error) {
	mode, userAgent, client, version, err := normalizeUAConfig(site.UAMode, site.CustomUserAgent, site.CustomClient, site.CustomVersion)
	if err != nil {
		return UAProfile{}, err
	}
	if mode == passthroughUAMode {
		return UAProfile{Name: "Passthrough", Passthrough: true}, nil
	}
	if mode == customUAMode {
		return UAProfile{Name: "Custom", UserAgent: userAgent, Client: client, Version: version}, nil
	}
	return uaProfiles[mode], nil
}

func mergeSiteUAConfig(old Site, requestedMode, requestedUserAgent, requestedClient, requestedVersion *string) (string, string, string, string, error) {
	hasCustomFields := requestedUserAgent != nil || requestedClient != nil || requestedVersion != nil
	if hasCustomFields && (requestedUserAgent == nil || requestedClient == nil || requestedVersion == nil) {
		return "", "", "", "", fmt.Errorf("custom User-Agent, Client, and Version must be provided together")
	}

	mode := old.UAMode
	userAgent := old.CustomUserAgent
	client := old.CustomClient
	version := old.CustomVersion
	if requestedMode != nil {
		mode = *requestedMode
	}
	if hasCustomFields {
		userAgent = *requestedUserAgent
		client = *requestedClient
		version = *requestedVersion
	}

	if requestedMode == nil && !hasCustomFields {
		return normalizeUAConfig(mode, userAgent, client, version)
	}

	normalizedMode := strings.ToLower(strings.TrimSpace(mode))
	if normalizedMode != customUAMode {
		if hasCustomFields && (strings.TrimSpace(userAgent) != "" || strings.TrimSpace(client) != "" || strings.TrimSpace(version) != "") {
			return "", "", "", "", fmt.Errorf("custom fields require ua_mode custom")
		}
		return normalizeUAConfig(normalizedMode, "", "", "")
	}
	if !hasCustomFields {
		return "", "", "", "", fmt.Errorf("custom ua_mode requires User-Agent, Client, and Version")
	}
	return normalizeUAConfig(normalizedMode, userAgent, client, version)
}

type embyAuthAttribute struct {
	name       string
	valueStart int
	valueEnd   int
}

func isEmbyAuthWhitespace(value byte) bool {
	return value == ' ' || value == '\t'
}

func isEmbyAuthToken(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '-' || value == '_'
}

func parseEmbyAuthorizationAttributes(value string, offset int) ([]embyAuthAttribute, bool) {
	attributes := make([]embyAuthAttribute, 0, 4)
	for {
		for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
			offset++
		}
		nameStart := offset
		for offset < len(value) && isEmbyAuthToken(value[offset]) {
			offset++
		}
		if nameStart == offset {
			return nil, false
		}
		name := value[nameStart:offset]
		for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
			offset++
		}
		if offset >= len(value) || value[offset] != '=' {
			return nil, false
		}
		offset++
		for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
			offset++
		}
		if offset >= len(value) || value[offset] != '"' {
			return nil, false
		}
		offset++
		valueStart := offset
		for offset < len(value) && value[offset] != '"' {
			if value[offset] == '\\' || value[offset] < 0x20 || value[offset] == 0x7f {
				return nil, false
			}
			offset++
		}
		if offset >= len(value) {
			return nil, false
		}
		attributes = append(attributes, embyAuthAttribute{
			name:       name,
			valueStart: valueStart,
			valueEnd:   offset,
		})
		offset++
		for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
			offset++
		}
		if offset == len(value) {
			return attributes, true
		}
		if value[offset] != ',' {
			return nil, false
		}
		offset++
		if offset == len(value) {
			return nil, false
		}
	}
}

func rewriteEmbyAuthorizationValue(value string, profile UAProfile) string {
	offset := 0
	for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
		offset++
	}
	schemeStart := offset
	for offset < len(value) && isEmbyAuthToken(value[offset]) {
		offset++
	}
	if schemeStart == offset {
		return value
	}
	scheme := value[schemeStart:offset]
	if !strings.EqualFold(scheme, "MediaBrowser") && !strings.EqualFold(scheme, "Emby") {
		return value
	}
	if offset < len(value) && !isEmbyAuthWhitespace(value[offset]) {
		return value
	}
	for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
		offset++
	}
	if offset == len(value) {
		prefix := value
		if len(value) == schemeStart+len(scheme) {
			prefix += " "
		}
		return prefix + "Client=\"" + profile.Client + "\", Version=\"" + profile.Version + "\""
	}

	attributes, ok := parseEmbyAuthorizationAttributes(value, offset)
	if !ok {
		return value
	}
	clientIndex, versionIndex := -1, -1
	for index, attribute := range attributes {
		switch {
		case strings.EqualFold(attribute.name, "Client"):
			if clientIndex >= 0 {
				return value
			}
			clientIndex = index
		case strings.EqualFold(attribute.name, "Version"):
			if versionIndex >= 0 {
				return value
			}
			versionIndex = index
		}
	}

	type replacement struct {
		start int
		end   int
		value string
	}
	replacements := make([]replacement, 0, 2)
	if clientIndex >= 0 {
		attribute := attributes[clientIndex]
		replacements = append(replacements, replacement{attribute.valueStart, attribute.valueEnd, profile.Client})
	}
	if versionIndex >= 0 {
		attribute := attributes[versionIndex]
		replacements = append(replacements, replacement{attribute.valueStart, attribute.valueEnd, profile.Version})
	}

	if len(replacements) == 2 && replacements[0].start < replacements[1].start {
		replacements[0], replacements[1] = replacements[1], replacements[0]
	}
	rewritten := value
	for _, replacement := range replacements {
		rewritten = rewritten[:replacement.start] + replacement.value + rewritten[replacement.end:]
	}
	if clientIndex < 0 {
		rewritten += ", Client=\"" + profile.Client + "\""
	}
	if versionIndex < 0 {
		rewritten += ", Version=\"" + profile.Version + "\""
	}
	return rewritten
}

func rewriteEmbyAuthorizationHeaders(header http.Header, headerName string, profile UAProfile) {
	for name, values := range header {
		if !strings.EqualFold(name, headerName) {
			continue
		}
		for index, value := range values {
			values[index] = rewriteEmbyAuthorizationValue(value, profile)
		}
	}
}

func applyUAProfileHeaders(header http.Header, profile UAProfile) {
	if profile.Passthrough {
		return // preserve the original request's User-Agent, Client, and Version
	}
	header.Set("User-Agent", profile.UserAgent)
	rewriteEmbyAuthorizationHeaders(header, "X-Emby-Authorization", profile)
	rewriteEmbyAuthorizationHeaders(header, "Authorization", profile)
}
