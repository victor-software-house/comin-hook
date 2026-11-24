# Lists what Funnel would publish besides the receiver.
# Input: the output of "tailscale serve status --json".
# Arguments: $port, the Funnel port, and $path, the mount of the receiver.
# It prints one message for each problem, and nothing when the port is free.
# Funnel applies to a whole host and port, so any other handler on the port
# would become public with the receiver.
def configs: ., ((.Foreground // {}) | .[]);

configs
| (
    ((.Web // {}) | to_entries[]
      | select(.key | endswith(":" + $port))
      | .key as $host
      | ((.value.Handlers // {}) | keys[])
      | select(. != $path)
      | "\($host) has a handler at \(.)"),
    (((.TCP // {})[$port] // empty)
      | select(.HTTP == true or ((.TCPForward // "") != "") or ((.TerminateTLS // "") != ""))
      | "port \($port) is not an HTTPS port for web handlers only")
  )
