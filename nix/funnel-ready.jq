# Tells whether the node is on the tailnet and knows its DNS name.
# Input: the output of "tailscale status --json".
.BackendState == "Running" and ((.Self.DNSName // "") != "")
