package rmq

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adjust/uniuri"
	"gopkg.in/redis.v5"
)

const (
	connectionsKey                   = "rmq::connections"                                           // Set of connection names
	connectionHeartbeatTemplate      = "rmq::connection::{connection}::heartbeat"                   // expires after {connection} died
	connectionQueuesTemplate         = "rmq::connection::{connection}::queues"                      // Set of queues consumers of {connection} are consuming
	connectionQueueConsumersTemplate = "rmq::connection::{connection}::queue::[{queue}]::consumers" // Set of all consumers from {connection} consuming from {queue}
	connectionQueueUnackedTemplate   = "rmq::connection::{connection}::queue::[{queue}]::unacked"   // List of deliveries consumers of {connection} are currently consuming

	queuesKey             = "rmq::queues"                     // Set of all open queues
	queueReadyTemplate    = "rmq::queue::[{queue}]::ready"    // List of deliveries in that {queue} (right is first and oldest, left is last and youngest)
	queueRejectedTemplate = "rmq::queue::[{queue}]::rejected" // List of rejected deliveries from that {queue}
	queueDelayedTemplate  = "rmq::queue::[{queue}]::delayed"  // List of rejected deliveries from that {queue}

	phConnection = "{connection}" // connection name
	phQueue      = "{queue}"      // queue name

	defaultBatchTimeout = time.Second
	purgeBatchSize      = 100
)

type Queue interface {
	Publish(payload string) bool
	PublishToDelayedQueue(payload string, delayedTime time.Duration) bool
	SetPushQueue(pushQueue Queue)
	StartConsuming(prefetchLimit int, pollDuration time.Duration) bool
	StopConsuming() bool
	WaitForConsuming()
	AddConsumer(tag string, consumer Consumer) string
	AddBatchConsumer(tag string, batchSize int, consumer BatchConsumer) string
	AddBatchConsumerWithTimeout(tag string, batchSize int, timeout time.Duration, consumer BatchConsumer) string
	PurgeReady() int
	PurgeRejected() int
	ReturnRejected(count int) int
	ReturnAllRejected() int
	Close() bool
}

type redisQueue struct {
	name           string
	connectionName string
	queuesKey      string // key to list of queues consumed by this connection
	consumersKey   string // key to set of consumers using this connection
	readyKey       string // key to list of ready deliveries
	delayedKey     string // key to list of delayed deliveries
	rejectedKey    string // key to list of rejected deliveries
	unackedKey     string // key to list of currently consuming deliveries
	pushKey        string // key to list of pushed deliveries
	redisClient    *redis.Client

	deliveryChan                chan Delivery // nil for publish channels, not nil for consuming channels
	deliveryChanForDelayedQueue chan Delivery // nil for publish channels, not nil for consuming channels

	consumerWaitGroup *sync.WaitGroup // WaitGroup to make sure that consuming finished in case of stop consuming

	// max number of prefetched deliveries number of unacked can go up to prefetchLimit + numConsumers
	prefetchLimit int

	pollDuration     time.Duration
	consumingStopped int32
}

func newQueue(name, connectionName, queuesKey string, redisClient *redis.Client) *redisQueue {
	consumersKey := strings.Replace(connectionQueueConsumersTemplate, phConnection, connectionName, 1)
	consumersKey = strings.Replace(consumersKey, phQueue, name, 1)

	readyKey := strings.Replace(queueReadyTemplate, phQueue, name, 1)
	delayedKey := strings.Replace(queueDelayedTemplate, phQueue, name, 1)
	rejectedKey := strings.Replace(queueRejectedTemplate, phQueue, name, 1)

	unackedKey := strings.Replace(connectionQueueUnackedTemplate, phConnection, connectionName, 1)
	unackedKey = strings.Replace(unackedKey, phQueue, name, 1)

	queue := &redisQueue{
		name:              name,
		connectionName:    connectionName,
		queuesKey:         queuesKey,
		consumersKey:      consumersKey,
		readyKey:          readyKey,
		delayedKey:        delayedKey,
		rejectedKey:       rejectedKey,
		unackedKey:        unackedKey,
		redisClient:       redisClient,
		consumerWaitGroup: new(sync.WaitGroup),
		consumingStopped:  0,
	}
	return queue
}

func (queue *redisQueue) increaseConsumerCount() {
	queue.consumerWaitGroup.Add(1)
}

func (queue *redisQueue) decreaseConsumerCount() {
	queue.consumerWaitGroup.Done()
}

func (queue *redisQueue) WaitForConsuming() {
	queue.consumerWaitGroup.Wait()
}

func (queue *redisQueue) String() string {
	return fmt.Sprintf("[%s conn:%s]", queue.name, queue.connectionName)
}

// Publish adds a delivery with the given payload to the queue
func (queue *redisQueue) Publish(payload string) bool {
	// debug(fmt.Sprintf("publish %s %s", payload, queue)) // COMMENTOUT
	return !redisErrIsNil(queue.redisClient.LPush(queue.readyKey, payload))
}

// PublishToDelayedQueue adds a delivery with the given payload to a delayed queue
func (queue *redisQueue) PublishToDelayedQueue(payload string, delayedTime time.Duration) bool {
	// debug(fmt.Sprintf("publish %s %s", payload, queue)) // COMMENTOUT
	return !redisErrIsNil(
		queue.redisClient.ZAdd(
			queue.delayedKey,
			redis.Z{
				Member: payload,
				Score:  float64(time.Now().Add(delayedTime).UnixNano()),
			},
		),
	)
}

// PurgeReady removes all ready deliveries from the queue and returns the number of purged deliveries
func (queue *redisQueue) PurgeReady() int {
	return queue.deleteRedisList(queue.readyKey)
}

// PurgeDelayed removes all delayed deliveries from the queue and returns the number of purged deliveries
func (queue *redisQueue) PurgeDelayed() int {
	return queue.deleteRedisZSet(queue.delayedKey)
}

// PurgeRejected removes all rejected deliveries from the queue and returns the number of purged deliveries
func (queue *redisQueue) PurgeRejected() int {
	return queue.deleteRedisList(queue.rejectedKey)
}

// Close purges and removes the queue from the list of queues
func (queue *redisQueue) Close() bool {
	queue.PurgeRejected()
	queue.PurgeDelayed()
	queue.PurgeReady()
	result := queue.redisClient.SRem(queuesKey, queue.name)
	if redisErrIsNil(result) {
		return false
	}
	return result.Val() > 0
}

func (queue *redisQueue) ReadyCount() int {
	result := queue.redisClient.LLen(queue.readyKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) DelayedCount() int {
	result := queue.redisClient.ZCount(queue.delayedKey, "-inf", "+inf")
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) UnackedCount() int {
	result := queue.redisClient.LLen(queue.unackedKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) RejectedCount() int {
	result := queue.redisClient.LLen(queue.rejectedKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

// ReturnAllUnacked moves all unacked deliveries back to the ready
// queue and deletes the unacked key afterwards, returns number of returned
// deliveries
func (queue *redisQueue) ReturnAllUnacked() int {
	result := queue.redisClient.LLen(queue.unackedKey)
	if redisErrIsNil(result) {
		return 0
	}

	unackedCount := int(result.Val())
	for i := 0; i < unackedCount; i++ {
		if redisErrIsNil(queue.redisClient.RPopLPush(queue.unackedKey, queue.readyKey)) {
			return i
		}
		// debug(fmt.Sprintf("rmq queue returned unacked delivery %s %s", result.Val(), queue.readyKey)) // COMMENTOUT
	}

	return unackedCount
}

// ReturnAllRejected moves all rejected deliveries back to the ready
// list and returns the number of returned deliveries
func (queue *redisQueue) ReturnAllRejected() int {
	result := queue.redisClient.LLen(queue.rejectedKey)
	if redisErrIsNil(result) {
		return 0
	}

	rejectedCount := int(result.Val())
	return queue.ReturnRejected(rejectedCount)
}

// ReturnRejected tries to return count rejected deliveries back to
// the ready list and returns the number of returned deliveries
func (queue *redisQueue) ReturnRejected(count int) int {
	if count == 0 {
		return 0
	}

	for i := 0; i < count; i++ {
		result := queue.redisClient.RPopLPush(queue.rejectedKey, queue.readyKey)
		if redisErrIsNil(result) {
			return i
		}
		// debug(fmt.Sprintf("rmq queue returned rejected delivery %s %s", result.Val(), queue.readyKey)) // COMMENTOUT
	}

	return count
}

// CloseInConnection closes the queue in the associated connection by removing all related keys
func (queue *redisQueue) CloseInConnection() {
	redisErrIsNil(queue.redisClient.Del(queue.unackedKey))
	redisErrIsNil(queue.redisClient.Del(queue.consumersKey))
	redisErrIsNil(queue.redisClient.SRem(queue.queuesKey, queue.name))
}

func (queue *redisQueue) SetPushQueue(pushQueue Queue) {
	redisPushQueue, ok := pushQueue.(*redisQueue)
	if !ok {
		return
	}

	queue.pushKey = redisPushQueue.readyKey
}

// StartConsuming starts consuming into a channel of size prefetchLimit
// must be called before consumers can be added!
// pollDuration is the duration the queue sleeps before checking for new deliveries
func (queue *redisQueue) StartConsuming(prefetchLimit int, pollDuration time.Duration) bool {
	if queue.deliveryChan != nil {
		return false // already consuming
	}

	// add queue to list of queues consumed on this connection
	if redisErrIsNil(queue.redisClient.SAdd(queue.queuesKey, queue.name)) {
		log.Panicf("rmq queue failed to start consuming %s", queue)
	}

	queue.prefetchLimit = prefetchLimit
	queue.pollDuration = pollDuration
	queue.deliveryChan = make(chan Delivery, prefetchLimit)
	queue.deliveryChanForDelayedQueue = make(chan Delivery, prefetchLimit)
	// log.Printf("rmq queue started consuming %s %d %s", queue, prefetchLimit, pollDuration)
	go queue.consume()
	go queue.consumeForDelayedQueue()
	return true
}

func (queue *redisQueue) StopConsuming() bool {
	if queue.deliveryChan == nil || queue.deliveryChanForDelayedQueue == nil || atomic.LoadInt32(&queue.consumingStopped) == 1 {
		return false // not consuming or already stopped
	}

	atomic.StoreInt32(&queue.consumingStopped, 1)
	return true
}

// AddConsumer adds a consumer to the queue and returns its internal name
// panics if StartConsuming wasn't called before!
func (queue *redisQueue) AddConsumer(tag string, consumer Consumer) string {
	name := queue.addConsumer(tag)
	go queue.consumerConsume(consumer)
	go queue.consumerConsumeDelayedQueue(consumer)
	return name
}

// AddBatchConsumer is similar to AddConsumer, but for batches of deliveries
func (queue *redisQueue) AddBatchConsumer(tag string, batchSize int, consumer BatchConsumer) string {
	return queue.AddBatchConsumerWithTimeout(tag, batchSize, defaultBatchTimeout, consumer)
}

func (queue *redisQueue) AddBatchConsumerWithTimeout(tag string, batchSize int, timeout time.Duration, consumer BatchConsumer) string {
	name := queue.addConsumer(tag)
	go queue.consumerBatchConsume(batchSize, timeout, consumer)
	go queue.consumerBatchConsumeDelayedQueue(batchSize, timeout, consumer)
	return name
}

func (queue *redisQueue) GetConsumers() []string {
	result := queue.redisClient.SMembers(queue.consumersKey)
	if redisErrIsNil(result) {
		return []string{}
	}
	return result.Val()
}

func (queue *redisQueue) RemoveConsumer(name string) bool {
	result := queue.redisClient.SRem(queue.consumersKey, name)
	if redisErrIsNil(result) {
		return false
	}
	return result.Val() > 0
}

func (queue *redisQueue) addConsumer(tag string) string {
	if queue.deliveryChan == nil {
		log.Panicf("rmq queue failed to add consumer, call StartConsuming first! %s", queue)
	}

	name := fmt.Sprintf("%s-%s", tag, uniuri.NewLen(6))

	// add consumer to list of consumers of this queue
	if redisErrIsNil(queue.redisClient.SAdd(queue.consumersKey, name)) {
		log.Panicf("rmq queue failed to add consumer %s %s", queue, tag)
	}

	// log.Printf("rmq queue added consumer %s %s", queue, name)
	return name
}

func (queue *redisQueue) RemoveAllConsumers() int {
	result := queue.redisClient.Del(queue.consumersKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) consume() {
	for {
		batchSize := queue.batchSize()
		wantMore := queue.consumeBatch(batchSize)

		if !wantMore {
			time.Sleep(queue.pollDuration)
		}

		if atomic.LoadInt32(&queue.consumingStopped) == 1 {
			close(queue.deliveryChan)
			// drain the channel
			for len(queue.deliveryChan) > 0 {
				<-queue.deliveryChan
			}
			// log.Printf("rmq queue stopped consuming %s", queue)
			return
		}
	}
}

func (queue *redisQueue) consumeForDelayedQueue() {
	for {
		batchSize := queue.batchSizeForDelayedQueue()
		wantMore := queue.consumeBatchForDelayedQueue(batchSize)

		if !wantMore {
			time.Sleep(queue.pollDuration)
		}

		if atomic.LoadInt32(&queue.consumingStopped) == 1 {
			close(queue.deliveryChanForDelayedQueue)
			// drain the channel
			for len(queue.deliveryChanForDelayedQueue) > 0 {
				<-queue.deliveryChanForDelayedQueue
			}
			// log.Printf("rmq queue stopped consuming %s", queue)
			return
		}
	}
}

func (queue *redisQueue) batchSize() int {
	prefetchCount := len(queue.deliveryChan)
	prefetchLimit := queue.prefetchLimit - prefetchCount
	// TODO: ignore ready count here and just return prefetchLimit?
	if readyCount := queue.ReadyCount(); readyCount < prefetchLimit {
		return readyCount
	}
	return prefetchLimit
}

func (queue *redisQueue) batchSizeForDelayedQueue() int {
	prefetchCount := len(queue.deliveryChanForDelayedQueue)
	prefetchLimit := queue.prefetchLimit - prefetchCount
	// TODO: ignore ready count here and just return prefetchLimit?
	if readyCount := queue.DelayedCount(); readyCount < prefetchLimit {
		return readyCount
	}
	return prefetchLimit
}

// consumeBatch tries to read batchSize deliveries, returns true if any and all were consumed
func (queue *redisQueue) consumeBatch(batchSize int) bool {
	if batchSize == 0 {
		return false
	}

	for i := 0; i < batchSize; i++ {
		result := queue.redisClient.RPopLPush(queue.readyKey, queue.unackedKey)
		if redisErrIsNil(result) {
			// debug(fmt.Sprintf("rmq queue consumed last batch %s %d", queue, i)) // COMMENTOUT
			return false
		}

		// debug(fmt.Sprintf("consume %d/%d %s %s", i, batchSize, result.Val(), queue)) // COMMENTOUT
		queue.deliveryChan <- newDelivery(
			result.Val(),
			queue.unackedKey,
			queue.delayedKey,
			queue.rejectedKey,
			queue.pushKey,
			queue.redisClient,
		)
	}

	// debug(fmt.Sprintf("rmq queue consumed batch %s %d", queue, batchSize)) // COMMENTOUT
	return true
}

func (queue *redisQueue) moveFromSortedSetToList(from string, to string, now time.Time, batchSize int) *redis.Cmd {
	return queue.redisClient.Eval(
		`-- Get all of the messages with an expired "score"...
local val = redis.call('zrangebyscore', KEYS[1], '-inf', ARGV[1])
-- If we have values in the array, we will remove batchSize of them from the first queue
-- and add them onto the destination queue in chunks of 100, which moves
-- batchSize of the appropriate messages onto the destination queue very safely.
if(next(val) ~= nil) then
    redis.call('zremrangebyrank', KEYS[1], 0, ARGV[2] - 1)
    for i = 1, ARGV[2], 100 do
        redis.call('lpush', KEYS[2], unpack(val, i, math.min(i+99, ARGV[2])))
    end
end
return val`,
		[]string{from, to},
		now.UnixNano(),
		batchSize,
	)
}

// consumeBatchForDelayedQueue tries to read batchSize deliveries, returns true if any and all were consumed
func (queue *redisQueue) consumeBatchForDelayedQueue(batchSize int) bool {
	if batchSize == 0 {
		return false
	}

	result := queue.moveFromSortedSetToList(queue.delayedKey, queue.unackedKey, time.Now(), batchSize)
	if redisErrIsNil(result) {
		// debug(fmt.Sprintf("rmq queue consumed last batch %s %d", queue, i)) // COMMENTOUT
		return false
	}

	values, ok := result.Val().([]interface{})
	if !ok {
		return false
	}

	for _, value := range values {
		payload, ok := value.(string)
		if !ok {
			return false
		}

		queue.deliveryChanForDelayedQueue <- newDelivery(
			payload,
			queue.unackedKey,
			queue.delayedKey,
			queue.rejectedKey,
			queue.pushKey,
			queue.redisClient,
		)
	}

	return true
}

func (queue *redisQueue) consumerConsume(consumer Consumer) {
	queue.increaseConsumerCount()
	defer queue.decreaseConsumerCount()
	for delivery := range queue.deliveryChan {
		// debug(fmt.Sprintf("consumer consume %s %s", delivery, consumer)) // COMMENTOUT
		consumer.Consume(delivery)
	}
}

func (queue *redisQueue) consumerConsumeDelayedQueue(consumer Consumer) {
	queue.increaseConsumerCount()
	defer queue.decreaseConsumerCount()
	for delivery := range queue.deliveryChanForDelayedQueue {
		// debug(fmt.Sprintf("consumer consume %s %s", delivery, consumer)) // COMMENTOUT
		consumer.Consume(delivery)
	}
}

func (queue *redisQueue) consumerBatchConsume(batchSize int, timeout time.Duration, consumer BatchConsumer) {
	batch := make([]Delivery, 0)
	timer := time.NewTimer(timeout)
	stopTimer(timer) // timer not active yet

	queue.increaseConsumerCount()
	defer queue.decreaseConsumerCount()
	for {
		select {
		case <-timer.C:
			// debug("batch timer fired") // COMMENTOUT
			// consume batch below

		case delivery, ok := <-queue.deliveryChan:
			if !ok {
				// debug("batch channel closed") // COMMENTOUT
				return
			}

			batch = append(batch, delivery)
			// debug(fmt.Sprintf("batch consume added delivery %d", len(batch))) // COMMENTOUT

			if len(batch) == 1 { // added first delivery
				timer.Reset(timeout) // set timer to fire
			}

			if len(batch) < batchSize {
				// debug(fmt.Sprintf("batch consume wait %d < %d", len(batch), batchSize)) // COMMENTOUT
				continue
			}

			// consume batch below
		}

		// debug(fmt.Sprintf("batch consume consume %d", len(batch))) // COMMENTOUT
		consumer.Consume(batch)

		batch = batch[:0] // reset batch
		stopTimer(timer)  // stop and drain the timer if it fired in between
	}
}

func (queue *redisQueue) consumerBatchConsumeDelayedQueue(batchSize int, timeout time.Duration, consumer BatchConsumer) {
	batch := make([]Delivery, 0)
	timer := time.NewTimer(timeout)
	stopTimer(timer) // timer not active yet

	queue.increaseConsumerCount()
	defer queue.decreaseConsumerCount()
	for {
		select {
		case <-timer.C:
			// debug("batch timer fired") // COMMENTOUT
			// consume batch below

		case delivery, ok := <-queue.deliveryChanForDelayedQueue:
			if !ok {
				// debug("batch channel closed") // COMMENTOUT
				return
			}

			batch = append(batch, delivery)
			// debug(fmt.Sprintf("batch consume added delivery %d", len(batch))) // COMMENTOUT

			if len(batch) == 1 { // added first delivery
				timer.Reset(timeout) // set timer to fire
			}

			if len(batch) < batchSize {
				// debug(fmt.Sprintf("batch consume wait %d < %d", len(batch), batchSize)) // COMMENTOUT
				continue
			}

			// consume batch below
		}

		// debug(fmt.Sprintf("batch consume consume %d", len(batch))) // COMMENTOUT
		consumer.Consume(batch)

		batch = batch[:0] // reset batch
		stopTimer(timer)  // stop and drain the timer if it fired in between
	}
}

func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}

	select {
	case <-timer.C:
	default:
	}
}

// return number of deleted list items
// https://www.redisgreen.net/blog/deleting-large-lists
func (queue *redisQueue) deleteRedisList(key string) int {
	llenResult := queue.redisClient.LLen(key)
	total := int(llenResult.Val())
	if total == 0 {
		return 0 // nothing to do
	}

	// delete elements without blocking
	for todo := total; todo > 0; todo -= purgeBatchSize {
		// minimum of purgeBatchSize and todo
		batchSize := purgeBatchSize
		if batchSize > todo {
			batchSize = todo
		}

		// remove one batch
		queue.redisClient.LTrim(key, 0, int64(-1-batchSize))
	}

	return total
}

// return number of deleted list items
// https://www.redisgreen.net/blog/deleting-large-lists
func (queue *redisQueue) deleteRedisZSet(key string) int {
	llenResult := queue.redisClient.ZCount(key, "-inf", "+inf")
	total := int(llenResult.Val())
	if total == 0 {
		return 0 // nothing to do
	}

	// delete elements without blocking
	for todo := total; todo > 0; todo -= purgeBatchSize {
		// minimum of purgeBatchSize and todo
		batchSize := purgeBatchSize
		if batchSize > todo {
			batchSize = todo
		}

		// remove one batch
		queue.redisClient.ZRemRangeByRank(key, 0, int64(batchSize))
	}

	return total
}

// redisErrIsNil returns false if there is no error, true if the result error is nil and panics if there's another error
func redisErrIsNil(result redis.Cmder) bool {
	switch result.Err() {
	case nil:
		return false
	case redis.Nil:
		return true
	default:
		log.Panicf("rmq redis error is not nil %#v", result.Err())
		return false
	}
}

//func debug(message string) {
//	log.Printf("rmq debug: %s", message) // COMMENTOUT
//}
