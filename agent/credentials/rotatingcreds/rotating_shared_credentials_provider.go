/*
Package rotatingcreds is a credential provider that will retrieve credentials from the shared credentials file.
It will handle new credentials being rotated into the shared credentials file by internally expiring
the credentials every minute (or as configured with RotationInterval) and re-grabbing the latest creds from the file.

	sess := session.New(&aws.Config{
		Credentials: rotatingcreds.NewCredentials("/root/.aws/credentials", "default"),
	})
	svc := s3.New(sess)
*/
package rotatingcreds

import (
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/cihub/seelog"
)

// DefaultRotationInterval is how frequently to expire and re-retrieve the credentials from file.
var DefaultRotationInterval = time.Minute

// RotatingSharedCredentialsProvider is a provider that retrieves credentials via the
// shared credentials provider, and adds the functionality of expiring and re-retrieving
// those credentials from the file.
type RotatingSharedCredentialsProvider struct {
	credentials.Expiry

	RotationInterval          time.Duration
	sharedCredentialsProvider *credentials.SharedCredentialsProvider
}

// NewCredentials TODO
func NewCredentials(filename, profile string) *credentials.Credentials {
	return credentials.NewCredentials(
		&RotatingSharedCredentialsProvider{
			RotationInterval: DefaultRotationInterval,
			sharedCredentialsProvider: &credentials.SharedCredentialsProvider{
				Filename: filename,
				Profile:  profile,
			},
		},
	)
}

// Retrieve will use the given filename and profile and retrieve AWS credentials.
func (p *RotatingSharedCredentialsProvider) Retrieve() (credentials.Value, error) {
	v, err := p.sharedCredentialsProvider.Retrieve()
	if err != nil {
		return v, err
	}
	p.SetExpiration(time.Now().Add(p.RotationInterval), 0)
	v.ProviderName = "RotatingSharedCredentialsProvider"
	seelog.Infof("Successfully got instance credentials. Access Key ID XXXX%s from %s",
		v.AccessKeyID[len(v.AccessKeyID)-4:], v.ProviderName)
	return v, err
}
