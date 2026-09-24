package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// These are stored ClientHello templates. API-key accounts use ordinary TLS;
// the account types that consume fingerprint templates are outside this product.
type tlsProfile struct {
	Name                string   `json:"name"`
	Description         *string  `json:"description"`
	GREASE              bool     `json:"enable_grease"`
	CipherSuites        []uint16 `json:"cipher_suites"`
	Curves              []uint16 `json:"curves"`
	PointFormats        []uint16 `json:"point_formats"`
	SignatureAlgorithms []uint16 `json:"signature_algorithms"`
	ALPNProtocols       []string `json:"alpn_protocols"`
	SupportedVersions   []uint16 `json:"supported_versions"`
	KeyShareGroups      []uint16 `json:"key_share_groups"`
	PSKModes            []uint16 `json:"psk_modes"`
	Extensions          []uint16 `json:"extensions"`
}

const tlsProfileLists = "cipher_suites,curves,point_formats,signature_algorithms,alpn_protocols,supported_versions,key_share_groups,psk_modes,extensions"
const tlsProfileColumns = "name,description,enable_grease," + tlsProfileLists

func (p *tlsProfile) validate() error {
	if strings.TrimSpace(p.Name) == "" || len([]rune(p.Name)) > 100 || strings.ContainsRune(p.Name, 0) {
		return bad("invalid TLS profile name")
	}
	if p.Description != nil && (len(*p.Description) > 10000 || strings.ContainsRune(*p.Description, 0)) {
		return bad("invalid TLS profile description")
	}
	for _, values := range []*[]uint16{&p.CipherSuites, &p.Curves, &p.PointFormats, &p.SignatureAlgorithms, &p.SupportedVersions, &p.KeyShareGroups, &p.PSKModes, &p.Extensions} {
		if len(*values) > 256 {
			return bad("TLS profile lists must not exceed 256 items")
		}
		if len(*values) == 0 {
			*values = nil // Empty lists retain the stored default-value semantics.
		}
	}
	if len(p.ALPNProtocols) > 256 {
		return bad("too many ALPN protocols")
	}
	for _, protocol := range p.ALPNProtocols {
		if protocol == "" || len(protocol) > 255 || strings.ContainsAny(protocol, "\x00\r\n") {
			return bad("invalid ALPN protocol")
		}
	}
	if len(p.ALPNProtocols) == 0 {
		p.ALPNProtocols = nil
	}
	return nil
}

func tlsProfileJSON(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, name := range strings.Split(tlsProfileLists, ",") {
		if len(fields[name]) == 0 || string(fields[name]) == "null" {
			fields[name] = json.RawMessage(`[]`)
		}
	}
	return json.Marshal(fields)
}

func (a *App) tlsProfiles(w http.ResponseWriter, r *http.Request) error {
	var id int64
	var err error
	if r.PathValue("id") != "" {
		if id, err = pathID(r); err != nil {
			return err
		}
	}
	if r.Method == "GET" {
		if id != 0 {
			raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(p) FROM tls_fingerprint_profiles p WHERE id=$1", id))
			if err != nil {
				return err
			}
			raw, err = tlsProfileJSON(raw)
			if err != nil {
				return err
			}
			return reply(w, raw)
		}
		rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(p) FROM tls_fingerprint_profiles p ORDER BY name,id")
		if err != nil {
			return err
		}
		raw, err := jsonRows(rows)
		if err != nil {
			return err
		}
		for i := range raw {
			if raw[i], err = tlsProfileJSON(raw[i]); err != nil {
				return err
			}
		}
		return reply(w, raw)
	}
	if r.Method == "DELETE" {
		result, err := a.DB.ExecContext(r.Context(), "DELETE FROM tls_fingerprint_profiles WHERE id=$1", id)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return missing()
		}
		return reply(w, map[string]string{"message": "Profile deleted successfully"})
	}
	var patch map[string]json.RawMessage
	if err = decode(w, r, &patch); err != nil {
		return err
	}
	if patch == nil {
		return bad("TLS profile must be a JSON object")
	}
	for field, value := range patch {
		if !slices.Contains(strings.Split(tlsProfileColumns, ","), field) {
			return bad("unsupported TLS profile field")
		}
		if string(value) == "null" {
			delete(patch, field) // Omitted/null retain values; [] explicitly clears.
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var p tlsProfile
	if id != 0 {
		raw, err := jsonRow(tx.QueryRowContext(r.Context(), "SELECT to_jsonb(p) FROM tls_fingerprint_profiles p WHERE id=$1 FOR UPDATE", id))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &p); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(patch)
	if json.Unmarshal(raw, &p) != nil {
		return bad("invalid TLS profile fields")
	}
	if err = p.validate(); err != nil {
		return err
	}
	raw, _ = json.Marshal(p)
	if id == 0 {
		err = tx.QueryRowContext(r.Context(), "INSERT INTO tls_fingerprint_profiles("+tlsProfileColumns+") SELECT "+tlsProfileColumns+" FROM jsonb_populate_record(NULL::tls_fingerprint_profiles,$1::jsonb) RETURNING id", string(raw)).Scan(&id)
	} else {
		_, err = tx.ExecContext(r.Context(), "UPDATE tls_fingerprint_profiles SET ("+tlsProfileColumns+")=(SELECT "+tlsProfileColumns+" FROM jsonb_populate_record(NULL::tls_fingerprint_profiles,$2::jsonb)),updated_at=clock_timestamp() WHERE id=$1", id, string(raw))
	}
	if err != nil {
		return err
	}
	result, err := jsonRow(tx.QueryRowContext(r.Context(), "SELECT to_jsonb(p) FROM tls_fingerprint_profiles p WHERE id=$1", id))
	if err != nil {
		return err
	}
	result, err = tlsProfileJSON(result)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, result)
}

func (a *App) tlsProfileRoutes() {
	for _, path := range []string{"", "/{id}"} {
		a.route("GET /api/v1/admin/tls-fingerprint-profiles"+path, "admin", a.tlsProfiles)
	}
	a.route("POST /api/v1/admin/tls-fingerprint-profiles", "admin", a.tlsProfiles)
	a.route("PUT /api/v1/admin/tls-fingerprint-profiles/{id}", "admin", a.tlsProfiles)
	a.route("DELETE /api/v1/admin/tls-fingerprint-profiles/{id}", "admin", a.tlsProfiles)
}
