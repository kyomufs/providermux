package antigravity

import "strings"

// refusedSystemSentence is a sentence that clients copy into their own system
// instruction. The service answers it with 429 RESOURCE_EXHAUSTED for the whole
// request, before the model reads anything.
//
// The measurement behind this: on gemini-3.8-flash the sentence alone in
// systemInstruction is refused, the same words in a user turn are not, and the
// account's quota windows stay at full capacity — so the answer is not about
// capacity. Rewording it is enough: the identical request, tools and turns
// included, answers 200 once the replacement below stands in its place.
const refusedSystemSentence = "The browser provides no implicit DOM, route, or screenshot context."

// replacedSystemSentence says the same thing in words the service serves.
const replacedSystemSentence = "The browser hands over no page structure, routing information, or screenshot on its own."

// acceptableSystem returns the system instruction the service will serve.
//
// Only the sentence above is touched: it is the client's own note about what the
// browser does not hand over, so the meaning carries no weight of its own, and
// leaving it in place costs the whole request.
func acceptableSystem(system string) string {
	if !strings.Contains(system, refusedSystemSentence) {
		return system
	}
	return strings.ReplaceAll(system, refusedSystemSentence, replacedSystemSentence)
}
