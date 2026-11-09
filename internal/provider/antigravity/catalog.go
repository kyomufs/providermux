package antigravity

import (
	"context"

	"free-ai-router/internal/provider"
)

// tier is one route of the service's model table: the name that goes into the
// `model` field, the enum label the service files the request under, and the
// thinking setting of that route.
//
// THE ROUTE AND THE SETTING ARE ONE THING. The service has no `gemini-3.8-flash`
// route: the effort is part of the model name, and each name carries its own
// thinking budget. Sending the base name and an effort separately is not a shape
// the service accepts.
type tier struct {
	// wire is the model name on the wire.
	wire string
	// enum is the label the request is filed under in the service's own
	// statistics.
	enum string
	// budget is the thinking budget. Zero means "do not send one" and a negative
	// value means the model decides for itself — the service reads the two
	// differently, so they are not folded together.
	budget int
	// level is the adaptive thinking level (Gemini 4 Argon and the Claude
	// routes). Empty means the route does not take one.
	level string
}

// modelDef is one model as the client sees it.
type modelDef struct {
	id        string
	name      string
	context   int
	maxOutput int
	vision    bool
	// claude marks the routes that take the stricter tool and thinking form.
	claude bool
	// defaultTier is used when the client names no effort. The client is not
	// required to: an OpenAI request has nowhere to put one.
	defaultTier string
	// tiers are the efforts this model offers.
	tiers map[string]tier
}

// modelTable is the service's model table, as published by its own client.
//
// The routes, the enum labels, the windows and the thinking budgets are copied
// from that table rather than invented: the service answers an unknown route by
// refusing the request, and an enum label that does not match the route files the
// statistics under a model that was never asked for.
//
// Source: @cortexkit/antigravity-auth-core 3.0.14, dist/model-registry.js
// (AGY_NATIVE_MODEL_DEFINITIONS).
func modelTable() []modelDef {
	return []modelDef{
		{
			id:          "gemini-4-argon",
			name:        "Gemini 4 Argon",
			context:     1_048_576,
			maxOutput:   65_536,
			vision:      true,
			defaultTier: "medium",
			tiers: map[string]tier{
				"low":    {wire: "gemini-4-argon-low", enum: "MODEL_PLACEHOLDER_M419", level: "LOW"},
				"medium": {wire: "gemini-4-argon-medium", enum: "MODEL_PLACEHOLDER_M418", level: "MEDIUM"},
				"high":   {wire: "gemini-4-argon-high", enum: "MODEL_PLACEHOLDER_M417", level: "HIGH"},
			},
		},
		{
			id:          "gemini-3.8-flash",
			name:        "Gemini 3.8 Flash",
			context:     1_048_576,
			maxOutput:   65_536,
			vision:      true,
			defaultTier: "medium",
			tiers: map[string]tier{
				"low":    {wire: "gemini-3.8-flash-low", enum: "MODEL_PLACEHOLDER_M320", budget: 1000},
				"medium": {wire: "gemini-3.8-flash-medium", enum: "MODEL_PLACEHOLDER_M319", budget: 4000},
				"high":   {wire: "gemini-3.8-flash-high", enum: "MODEL_PLACEHOLDER_M318", budget: -1},
			},
		},
		{
			id:          "claude-opus-5-5",
			name:        "Claude Opus 5.5",
			context:     1_000_000,
			maxOutput:   128_000,
			vision:      true,
			claude:      true,
			defaultTier: "medium",
			tiers: map[string]tier{
				"low":    {wire: "claude-opus-5-5-low", enum: "MODEL_PLACEHOLDER_M400", level: "LOW"},
				"medium": {wire: "claude-opus-5-5-medium", enum: "MODEL_PLACEHOLDER_M401", level: "MEDIUM"},
				"high":   {wire: "claude-opus-5-5-high", enum: "MODEL_PLACEHOLDER_M402", level: "HIGH"},
			},
		},
		{
			id:          "claude-sonnet-5-5",
			name:        "Claude Sonnet 5.5",
			context:     1_000_000,
			maxOutput:   128_000,
			vision:      true,
			claude:      true,
			defaultTier: "medium",
			tiers: map[string]tier{
				"low":    {wire: "claude-sonnet-5-5-low", enum: "MODEL_PLACEHOLDER_M403", level: "LOW"},
				"medium": {wire: "claude-sonnet-5-5-medium", enum: "MODEL_PLACEHOLDER_M404", level: "MEDIUM"},
				"high":   {wire: "claude-sonnet-5-5-high", enum: "MODEL_PLACEHOLDER_M405", level: "HIGH"},
			},
		},
		{
			id:          "gpt-oss-120b-medium",
			name:        "GPT-OSS 120B",
			context:     131_072,
			maxOutput:   32_768,
			vision:      false,
			defaultTier: "medium",
			tiers: map[string]tier{
				"medium": {wire: "gpt-oss-120b-medium", enum: "MODEL_OPENAI_GPT_OSS_120B_MEDIUM", budget: 8192},
			},
		},
	}
}

// effortLadder is the effort values offered to a client, in the order a ladder is
// listed in.
//
// The order is a property of the ladder rather than of a model: a client reading
// a list expects it to climb, and the table's map has no order at all.
var effortLadder = []string{"low", "medium", "high"}

// Models implements provider.Provider.
//
// The catalog is a constant: the service publishes no model list to the client,
// and the table above is what its own client ships with. Nothing is fetched, so
// the call never fails and never blocks.
func (p *Provider) Models(context.Context) ([]provider.Model, error) {
	table := modelTable()
	out := make([]provider.Model, 0, len(table))
	for _, def := range table {
		// The ladder offered is the one the model actually has, not the shared
		// three-step one: a model with a single route must not advertise an
		// effort that would be answered by silence.
		values := make([]string, 0, len(effortLadder))
		for _, effort := range effortLadder {
			if _, ok := def.tiers[effort]; ok {
				values = append(values, effort)
			}
		}
		out = append(out, provider.Model{
			ID:          def.id,
			Name:        def.name,
			Description: def.name + " (Google Antigravity)",
			Context:     def.context,
			MaxOutput:   def.maxOutput,
			Vision:      def.vision,
			Reasoning:   len(values) > 0,
			ReasoningOptions: []provider.ReasoningOption{{
				Kind:   provider.ReasoningEffort,
				Values: values,
			}},
			// The thinking of these routes arrives as parts flagged `thought`,
			// and the gateway only separates it for a client that was told the
			// field. Gemini and Claude both answer with reasoning_content
			// through the OpenAI surface.
			ReasoningField: "reasoning_content",
			Free:           true,
		})
	}
	return out, nil
}

// lookup finds a model by the identifier the client sent.
func lookup(id string) (modelDef, bool) {
	for _, def := range modelTable() {
		if def.id == id {
			return def, true
		}
	}
	return modelDef{}, false
}

// route resolves the effort of a request to a wire route.
//
// An unknown effort falls back to the model's default rather than to an error:
// the gateway clamps the effort against the declared ladder, so a value that
// arrives here unknown is a model that was asked for no effort at all.
func (def modelDef) route(effort string) tier {
	if route, ok := def.tiers[effort]; ok {
		return route
	}
	return def.tiers[def.defaultTier]
}
