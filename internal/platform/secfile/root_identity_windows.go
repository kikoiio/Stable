//go:build windows

package secfile

func captureRootIdentity(path string) (rootIdentitySnapshot, error) {
	signature, err := windowsRootIdentity(path)
	if err != nil {
		return rootIdentitySnapshot{}, err
	}
	return rootIdentitySnapshot{signature: signature}, nil
}

func sameRootIdentity(a, b rootIdentitySnapshot) bool {
	return a.signature != "" && a.signature == b.signature
}
