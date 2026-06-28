/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package seaweedfs

import (
	"context"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// newS3Client builds a minio S3 client for the given endpoint (host:port, no
// scheme) using the admin credentials over plain HTTP.
func newS3Client(endpoint, accessKey, secretKey string) (*minio.Client, error) {
	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
}

// ensureBucket creates the bucket if it does not already exist (idempotent).
func ensureBucket(ctx context.Context, mc *minio.Client, name string) error {
	exists, err := mc.BucketExists(ctx, name)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", name, err)
	}
	if exists {
		return nil
	}
	if err := mc.MakeBucket(ctx, name, minio.MakeBucketOptions{Region: s3Region}); err != nil {
		// Tolerate a race where the bucket appeared between the check and now.
		if exists2, errCheck := mc.BucketExists(ctx, name); errCheck == nil && exists2 {
			return nil
		}
		return fmt.Errorf("create bucket %q: %w", name, err)
	}
	return nil
}

// deleteBucket empties and removes the bucket (best effort). Used only when the
// ObjectBucket reclaim policy is Delete.
func deleteBucket(ctx context.Context, mc *minio.Client, name string) error {
	exists, err := mc.BucketExists(ctx, name)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", name, err)
	}
	if !exists {
		return nil
	}

	objects := mc.ListObjects(ctx, name, minio.ListObjectsOptions{Recursive: true})
	for rerr := range mc.RemoveObjects(ctx, name, objects, minio.RemoveObjectsOptions{}) {
		if rerr.Err != nil {
			return fmt.Errorf("empty bucket %q: %w", name, rerr.Err)
		}
	}

	if err := mc.RemoveBucket(ctx, name); err != nil {
		return fmt.Errorf("remove bucket %q: %w", name, err)
	}
	return nil
}
