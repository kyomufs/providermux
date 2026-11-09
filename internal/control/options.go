package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"providermux/internal/config"
	"providermux/internal/provider"
)

// UpdateOptionRequest defines an on-the-fly option modification request.
type UpdateOptionRequest struct {
	Axis     string `json:"axis"`
	Provider string `json:"provider,omitempty"`
	Value    string `json:"value"`
}

// CurrentOptions reflects the live configuration values in force.
type CurrentOptions struct {
	Mode                 string                    `json:"mode"`
	Credential           string                    `json:"credential"`
	KeyRotation          string                    `json:"key_rotation"`
	ModelFallback        string                    `json:"model_fallback"`
	Family               string                    `json:"family"`
	Transport            string                    `json:"transport"`
	FirstEventTimeout    string                    `json:"first_event_timeout"`
	IdleTimeout          string                    `json:"idle_timeout"`
	ReasoningIdleTimeout string                    `json:"reasoning_idle_timeout"`
	Providers            map[string]ProviderOption `json:"providers"`
}

// ProviderOption reflects live options for a specific provider.
type ProviderOption struct {
	Credential  string `json:"credential"`
	KeyRotation string `json:"key_rotation"`
	Transport   string `json:"transport"`
}

func (h *Handler) currentOptions() *CurrentOptions {
	if h.Config == nil {
		return nil
	}
	cur := &CurrentOptions{
		Mode:                 h.Config.Mode.Val,
		Credential:           h.Config.Credential.Val,
		KeyRotation:          h.Config.KeyRotation.Val,
		ModelFallback:        h.Config.ModelFallback.Val,
		Family:               h.Config.Family.Val,
		Transport:            h.Config.Transport.Val,
		FirstEventTimeout:    h.Config.FirstEventTimeout.Val.String(),
		IdleTimeout:          h.Config.IdleTimeout.Val.String(),
		ReasoningIdleTimeout: h.Config.ReasoningIdleTimeout.Val.String(),
		Providers:            map[string]ProviderOption{},
	}
	if h.Providers != nil {
		for _, id := range h.Providers.IDs() {
			sID := string(id)
			cur.Providers[sID] = ProviderOption{
				Credential:  h.Config.CredentialFor(sID),
				KeyRotation: h.Config.RotationFor(sID),
				Transport:   h.Config.TransportFor(sID),
			}
		}
	}
	return cur
}

// handleOptionsUpdate processes on-the-fly changes to settings.
func (h *Handler) handleOptionsUpdate(w http.ResponseWriter, r *http.Request) {
	var req UpdateOptionRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}

	if req.Axis == "" || req.Value == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "axis and value are required"})
		return
	}

	if h.Config == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "configuration is not loaded"})
		return
	}

	if req.Provider != "" {
		pID := provider.ID(req.Provider)
		if h.Providers != nil {
			if _, err := h.Providers.Get(pID); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown provider %q", req.Provider)})
				return
			}
		}

		if h.Config.Providers == nil {
			h.Config.Providers = map[string]config.ProviderConfig{}
		}
		pc := h.Config.Providers[req.Provider]

		switch req.Axis {
		case "credential":
			if !sliceContains(config.CredentialValues(), req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid credential value %q", req.Value)})
				return
			}
			pc.Credential = req.Value
		case "key_rotation":
			if !sliceContains(config.RotationValues(), req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid key_rotation value %q", req.Value)})
				return
			}
			pc.KeyRotation = req.Value
		case "transport":
			if len(h.Transports) > 0 && !sliceContains(h.Transports, req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid transport value %q", req.Value)})
				return
			}
			pc.Transport = req.Value
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unsupported provider axis %q", req.Axis)})
			return
		}

		h.Config.Providers[req.Provider] = pc
		h.info("provider %s axis %s updated to %s", req.Provider, req.Axis, req.Value)

		// Persist to config.json if path is set
		if h.Config.Paths.ConfigFile != "" {
			_ = persistProviderOption(h.Config.Paths.ConfigFile, req.Provider, req.Axis, req.Value)
		}
	} else {
		// Global axis update
		switch req.Axis {
		case "mode":
			if !sliceContains(config.ModeValues(), req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid mode %q", req.Value)})
				return
			}
			h.Config.Mode.Set(req.Value, config.SourceFile)
		case "credential":
			if !sliceContains(config.CredentialValues(), req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid credential %q", req.Value)})
				return
			}
			h.Config.Credential.Set(req.Value, config.SourceFile)
		case "key_rotation":
			if !sliceContains(config.RotationValues(), req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid key_rotation %q", req.Value)})
				return
			}
			h.Config.KeyRotation.Set(req.Value, config.SourceFile)
		case "model_fallback":
			if !sliceContains(config.FallbackValues(), req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid model_fallback %q", req.Value)})
				return
			}
			h.Config.ModelFallback.Set(req.Value, config.SourceFile)
		case "family":
			if !sliceContains(config.FamilyValues(), req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid family %q", req.Value)})
				return
			}
			h.Config.Family.Set(req.Value, config.SourceFile)
		case "transport":
			if len(h.Transports) > 0 && !sliceContains(h.Transports, req.Value) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid transport %q", req.Value)})
				return
			}
			h.Config.Transport.Set(req.Value, config.SourceFile)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unsupported global axis %q", req.Axis)})
			return
		}

		h.info("global axis %s updated to %s", req.Axis, req.Value)
		if h.Config.Paths.ConfigFile != "" {
			_ = persistGlobalOption(h.Config.Paths.ConfigFile, req.Axis, req.Value)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"updated": true,
		"axis":    req.Axis,
		"value":   req.Value,
		"current": h.currentOptions(),
	})
}

func sliceContains(s []string, val string) bool {
	for _, item := range s {
		if strings.EqualFold(item, val) {
			return true
		}
	}
	return false
}

func persistGlobalOption(path, key, value string) error {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &doc)
	}
	doc[key] = value
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return writeAtomic(path, out)
}

func persistProviderOption(path, prov, key, value string) error {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &doc)
	}
	provs, ok := doc["providers"].(map[string]any)
	if !ok || provs == nil {
		provs = map[string]any{}
		doc["providers"] = provs
	}
	pMap, ok := provs[prov].(map[string]any)
	if !ok || pMap == nil {
		pMap = map[string]any{}
		provs[prov] = pMap
	}
	pMap[key] = value

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return writeAtomic(path, out)
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}
