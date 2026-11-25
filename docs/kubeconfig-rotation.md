# Kubeconfig credential rotation

darwin-node loads its kubeconfig once, through
`nodeutil.ClientsetFromEnv`, when the process joins the cluster. client-go
keeps that rest config for the life of the process. Replacing the file
on disk does not rotate the credential the process is using.

To rotate the client credential:

1. Write the new kubeconfig to the path in `KUBECONFIG` (or `--kubeconfig`).
   An atomic rename avoids a reader seeing a partial file on the next start.
2. Restart the process. Under the supplied launchd plist, `KeepAlive` starts
   it again after it exits.
3. SIGTERM still runs the drain. `ExitTimeOut` has to stay above
   `--shutdown-grace-period` or launchd kills the process before a
   cache that already exited 0 is committed. See [installation.md](installation.md).

What already reloads without a restart:

- The kubelet serving certificate and key
  (`APISERVER_CERT_LOCATION`, `APISERVER_KEY_LOCATION`). The handshake
  calls `GetCertificate`, which reloads the files when the mtime or size
  changes and keeps the last pair that parsed.
- The kubelet client CA (`APISERVER_CA_CERT_LOCATION`). The process
  watches that file and uses the new bundle on the next handshake.

There is no CertificateSigningRequest bootstrap. A new client certificate
is a new kubeconfig and a restart.
