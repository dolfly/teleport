// Teleport
// Copyright (C) 2025 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package recordingencryption_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth/recordingencryption"
)

func TestEncryptedIO(t *testing.T) {
	ctx := t.Context()
	keyStore := newFakeKeyStore(types.PrivateKeyType_RAW)
	_, publicKey, err := keyStore.createKey()
	require.NoError(t, err)

	srcGetter, err := newFakeSRCGetter(true, []*types.AgeEncryptionKey{
		{
			PublicKey: publicKey,
		},
	})
	require.NoError(t, err)

	encryptedIO, err := recordingencryption.NewEncryptedIO(srcGetter, keyStore)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	writer, err := encryptedIO.WithEncryption(ctx, &writeCloser{Writer: out})
	require.NoError(t, err)

	msg := []byte("testing encrypted IO")
	_, err = writer.Write(msg)
	require.NoError(t, err)

	// writer must be closed to ensure data is flushed
	err = writer.Close()
	require.NoError(t, err)

	reader, err := encryptedIO.WithDecryption(ctx, out)
	require.NoError(t, err)

	plaintext, err := io.ReadAll(reader)
	require.NoError(t, err)

	require.Equal(t, msg, plaintext)

	// creating an EncryptedIO without a SessionRecordingConfigGetter or keyfinder should be an error
	_, err = recordingencryption.NewEncryptedIO(nil, nil)
	require.Error(t, err)
	_, err = recordingencryption.NewEncryptedIO(srcGetter, nil)
	require.Error(t, err)

	// wrapping encryption when encryption is disabled should return an ErrEncryptionDisabled
	srcGetter, err = newFakeSRCGetter(false, nil)
	require.NoError(t, err)
	encryptedIO, err = recordingencryption.NewEncryptedIO(srcGetter, keyStore)
	require.NoError(t, err)

	_, err = encryptedIO.WithEncryption(ctx, &writeCloser{Writer: out})
	require.ErrorIs(t, err, recordingencryption.ErrEncryptionDisabled)
}

type fakeSRCGetter struct {
	config types.SessionRecordingConfig
}

func newFakeSRCGetter(encrypted bool, keys []*types.AgeEncryptionKey) (*fakeSRCGetter, error) {
	spec := types.SessionRecordingConfigSpecV2{
		Encryption: &types.SessionRecordingEncryptionConfig{
			Enabled: encrypted,
		},
	}

	config, err := types.NewSessionRecordingConfigFromConfigFile(spec)
	if err != nil {
		return nil, err
	}

	config.SetEncryptionKeys(slices.Values(keys))

	return &fakeSRCGetter{
		config: config,
	}, nil
}

func (f *fakeSRCGetter) GetSessionRecordingConfig(ctx context.Context) (types.SessionRecordingConfig, error) {
	return f.config, nil
}

type writeCloser struct {
	io.Writer
}

func (w *writeCloser) Close() error {
	return nil
}
