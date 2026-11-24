# Tells whether Funnel is on for the port with a proxy handler for the receiver.
# Input: the output of "tailscale serve status --json".
# Arguments: $port, the Funnel port, $path, the mount of the receiver, and
# $target, the URL that the handler proxies to.
. as $config
| [($config.AllowFunnel // {}) | to_entries[] | select(.value == true and (.key | endswith(":" + $port))) | .key]
| any(.[]; (($config.Web // {})[.].Handlers // {})[$path].Proxy == $target)
