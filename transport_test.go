package dmsg

import (
	"testing"

	"github.com/skycoin/skycoin/src/util/logging"
	"github.com/stretchr/testify/assert"

	"github.com/skycoin/dmsg/cipher"
)

func TestNewTransport(t *testing.T) {
	log := logging.MustGetLogger("dmsg_test")
	tr := NewTransport(nil, log, cipher.PubKey{}, cipher.PubKey{}, 0, func(id uint16) {})
	assert.NotNil(t, tr)
}

func TestTransport_close(t *testing.T) {
	log := logging.MustGetLogger("dmsg_test")
	tr := NewTransport(nil, log, cipher.PubKey{}, cipher.PubKey{}, 0, func(id uint16) {})

	closed := tr.close()

	t.Run("Valid close() result (1st attempt)", func(t *testing.T) {
		assert.True(t, closed)
	})

	t.Run("Channel closed (1st attempt)", func(t *testing.T) {
		_, ok := <-tr.done
		assert.False(t, ok)
	})

	closed = tr.close()

	t.Run("Valid close() result (2nd attempt)", func(t *testing.T) {
		assert.False(t, closed)
	})

	t.Run("Channel closed (2nd attempt)", func(t *testing.T) {
		_, ok := <-tr.done
		assert.False(t, ok)
	})

	t.Run("No panic with nil pointer receiver", func(t *testing.T) {
		var tr1, tr2 *Transport
		assert.Nil(t, tr1.Close())
		assert.False(t, tr2.close())
	})
}
