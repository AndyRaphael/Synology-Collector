# NinjaOne integration

The collector is RMM-agnostic — it prints a `KEY=VALUE` block and returns an
exit code — but ships with a ready-to-use NinjaOne wrapper.

See [`examples/ninjaone.ps1`](../examples/ninjaone.ps1). It:

1. Reads host/credentials from NinjaOne environment variables (or custom fields).
2. Runs the collector and writes the full output to the **activity log** for
   troubleshooting.
3. Optionally maps KV lines to **custom fields** via `Ninja-Property-Set`
   (enable `$MapCustomFields` and adjust `$FieldMap`).
4. Exits with the collector's exit code, so a NinjaOne **condition** on script
   result (or on a mapped custom field) can notify a technician or open a ticket.

## Detecting "the collector hasn't run recently"

This is best handled RMM-side: map `COLLECTED_AT` to a custom field and add a
NinjaOne condition that fires when that field is older than your schedule
interval. That covers the case where the scheduled task itself stops running.

## Using another RMM

Any RMM that can run a binary on a schedule and read its exit code works. Point
your platform at the same `KEY=VALUE` and exit-code contract documented in
[Output & exit codes](output.md); the NinjaOne script is just a reference
implementation of that pattern.
