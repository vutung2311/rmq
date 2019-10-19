package rmq

import (
	"fmt"
	"time"

	"github.com/go-redis/redis"
)

type Delivery interface {
	Payload() string
	Ack() bool
	Delay(time.Duration) bool
	Reject() bool
	Push() bool
}

type wrapDelivery struct {
	payload     string
	unackedKey  string
	delayedKey  string
	rejectedKey string
	pushKey     string
	redisClient redis.UniversalClient
}

func newDelivery(payload, unackedKey, delayedKey, rejectedKey, pushKey string, redisClient redis.UniversalClient) *wrapDelivery {
	return &wrapDelivery{
		payload:     payload,
		unackedKey:  unackedKey,
		delayedKey:  delayedKey,
		rejectedKey: rejectedKey,
		pushKey:     pushKey,
		redisClient: redisClient,
	}
}

func (delivery *wrapDelivery) String() string {
	return fmt.Sprintf("[%s %s]", delivery.payload, delivery.unackedKey)
}

func (delivery *wrapDelivery) Payload() string {
	return delivery.payload
}

func (delivery *wrapDelivery) Ack() bool {
	// debug(fmt.Sprintf("delivery ack %s", delivery)) // COMMENTOUT

	result := delivery.redisClient.LRem(delivery.unackedKey, 1, delivery.payload)
	if redisErrIsNil(result) {
		return false
	}

	return result.Val() == 1
}

func (delivery *wrapDelivery) Delay(duration time.Duration) bool {
	zAddResult := delivery.redisClient.ZAdd(
		delivery.delayedKey,
		redis.Z{
			Score:  float64(time.Now().Add(duration).UnixNano()),
			Member: delivery.payload,
		},
	)
	if redisErrIsNil(zAddResult) {
		return false
	}

	lRemResult := delivery.redisClient.LRem(delivery.unackedKey, 1, delivery.payload)
	if redisErrIsNil(lRemResult) {
		return false
	}

	return zAddResult.Val() == 1 && lRemResult.Val() == 1
}

func (delivery *wrapDelivery) Reject() bool {
	return delivery.move(delivery.rejectedKey)
}

func (delivery *wrapDelivery) Push() bool {
	if delivery.pushKey != "" {
		return delivery.move(delivery.pushKey)
	} else {
		return delivery.move(delivery.rejectedKey)
	}
}

func (delivery *wrapDelivery) move(key string) bool {
	if redisErrIsNil(delivery.redisClient.LPush(key, delivery.payload)) {
		return false
	}

	if redisErrIsNil(delivery.redisClient.LRem(delivery.unackedKey, 1, delivery.payload)) {
		return false
	}

	// debug(fmt.Sprintf("delivery rejected %s", delivery)) // COMMENTOUT
	return true
}
