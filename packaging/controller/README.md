# Controller host bundle

`orch cluster install` embeds this unit and installs the invoked executable at
`/opt/orch-controller/bin/orch`. Controller data belongs to the dedicated
`orch-controller` account under `/var/lib/orch-controller`; the unit and binary
remain root-owned. This path is deliberately separate from the node bundle so
both roles can run on one supported host.
