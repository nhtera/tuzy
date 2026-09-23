package update

// ReleaseKeys are the base64 ed25519 public keys allowed to sign checksums.txt. Rotation: ship a
// release signed by the old key that lists both, then drop the old key.
var ReleaseKeys = []string{
	"tQiuluMwH12IIzLX4kVjYKu/+DKbRpKpa/Rn/pCyd5o=", // 2026-09-24, primary
}
