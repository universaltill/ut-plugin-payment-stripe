# Code review: ut-docs#6440 Stripe early decline on reader (2026-11-17)
Independent review by an Opus subagent (code written by Sonnet); tester report /tmp/tabletfix/tester-sumup-stripe.md (mutation-tested, 7/9 caught, the 2 gaps closed or equivalent).

Shipped: poll reads last_payment_error; declines at once only when the PaymentIntent is requires_payment_method with an error AND the reader action status is failed (cancel reader + PI first; a tap that slipped in is approved).
Findings: HIGH (unconfirmed) early decline during a soft-decline chip-and-PIN prompt — FIXED by requiring reader action failed; test TestSoftDeclineKeepsPollingAndApprovesLaterTap. Live reader verification deferred (blocked:env on #6440).
Verdict: approve after fix.

Pre-flight: review record updated; README + CLAUDE.md updated; manifest version bumped to 1.5.0; en.json keys n/a (no new labelled setting); help/ui n/a (plugin settings only); live sandbox/reader verification deferred -> ut-docs#6561 / #6440 (blocked:env items).
Re-verified myself: making readerActionFailed always return true makes TestSoftDeclineKeepsPollingAndApprovesLaterTap fail; restored, passes. Second review of the fix: approve; accepted LOW: failed action not tied to this PaymentIntent (reader shared by two tills), reason may come from a stale last_payment_error (prefer action.failure_code).
