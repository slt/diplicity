package game

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zond/diplicity/auth"
	"github.com/zond/go-fcm"
	"golang.org/x/net/context"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/urlfetch"

	. "github.com/zond/goaeoas"
)

const (
	fcmConfKind = "FCMConf"
	prodKey     = "prod"
)

func init() {
	FCMSendToTokensFunc = NewDelayFunc("game-fcmSendToTokens", fcmSendToTokens)
	manageFCMTokensFunc = NewDelayFunc("game-manageFCMTokens", manageFCMTokens)
}

var (
	FCMSendToTokensFunc *DelayFunc
	manageFCMTokensFunc *DelayFunc
	prodFCMConf         *FCMConf
	prodFCMConfLock     = sync.RWMutex{}
)

type FCMConf struct {
	ServerKey string
}

func getFCMConfKey(ctx context.Context) *datastore.Key {
	return datastore.NewKey(ctx, fcmConfKind, prodKey, 0, nil)
}

func SetFCMConf(ctx context.Context, fcmConf *FCMConf) error {
	return datastore.RunInTransaction(ctx, func(ctx context.Context) error {
		currentFCMConf := &FCMConf{}
		if err := datastore.Get(ctx, getFCMConfKey(ctx), currentFCMConf); err == nil {
			return HTTPErr{"FCMConf already configured", http.StatusBadRequest}
		}
		if _, err := datastore.Put(ctx, getFCMConfKey(ctx), fcmConf); err != nil {
			return err
		}
		return nil
	}, &datastore.TransactionOptions{XG: false})
}

func getFCMConf(ctx context.Context) (*FCMConf, error) {
	prodFCMConfLock.RLock()
	if prodFCMConf != nil {
		defer prodFCMConfLock.RUnlock()
		return prodFCMConf, nil
	}
	prodFCMConfLock.RUnlock()
	prodFCMConfLock.Lock()
	defer prodFCMConfLock.Unlock()
	foundConf := &FCMConf{}
	if err := datastore.Get(ctx, getFCMConfKey(ctx), foundConf); err != nil {
		return nil, err
	}
	prodFCMConf = foundConf
	return prodFCMConf, nil
}

func mutateFCMTokens(ctx context.Context, toMutate map[string]map[string]string, mutator func(*auth.FCMToken, string), cont func() error) error {
	return datastore.RunInTransaction(ctx, func(ctx context.Context) error {
		userConfigs := make([]auth.UserConfig, len(toMutate))
		ids := make([]*datastore.Key, 0, len(toMutate))
		for uid := range toMutate {
			ids = append(ids, auth.UserConfigID(ctx, auth.UserID(ctx, uid)))
		}
		if err := datastore.GetMulti(ctx, ids, userConfigs); err != nil {
			return err
		}
		for i := range userConfigs {
			conf := &userConfigs[i]
			userTokens := toMutate[conf.UserId]
			for j := range conf.FCMTokens {
				fcmToken := &conf.FCMTokens[j]
				if data, found := userTokens[fcmToken.Value]; found {
					mutator(fcmToken, data)
				}
			}
		}
		if _, err := datastore.PutMulti(ctx, ids, userConfigs); err != nil {
			return err
		}
		if cont != nil {
			return cont()
		}
		return nil
	}, &datastore.TransactionOptions{XG: true})
}

func splitMap(at int, m map[string]map[string]string) (m1, m2 map[string]map[string]string) {
	m1 = map[string]map[string]string{}
	m2 = map[string]map[string]string{}
	for uid, sm := range m {
		if len(m1) < at {
			m1[uid] = sm
		} else {
			m2[uid] = sm
		}
	}
	return m1, m2
}

func manageFCMTokens(ctx context.Context, tokensToRemove, tokensToUpdate map[string]map[string]string) error {
	log.Infof(ctx, "manageFCMTokens(..., %+v, %+v)", PP(tokensToRemove), PP(tokensToUpdate))

	if len(tokensToRemove) > 0 {
		toRemove, toDelay := splitMap(4, tokensToRemove)
		return mutateFCMTokens(
			ctx,
			toRemove,
			func(tok *auth.FCMToken, errMsg string) {
				tok.Disabled = true
				tok.Note = errMsg
			},
			func() error {
				if len(toDelay) > 0 || len(tokensToUpdate) > 0 {
					return manageFCMTokensFunc.EnqueueIn(ctx, 0, toDelay, tokensToUpdate)
				}
				return nil
			},
		)
	}

	if len(tokensToUpdate) > 0 {
		toUpdate, toDelay := splitMap(4, tokensToUpdate)
		return mutateFCMTokens(
			ctx,
			toUpdate,
			func(tok *auth.FCMToken, newValue string) {
				tok.Note = fmt.Sprintf("Updated from %q at %v due to FCM service indication.", tok.Value, time.Now())
				tok.Value = newValue
			},
			func() error {
				if len(toDelay) > 0 || len(tokensToUpdate) > 0 {
					return manageFCMTokensFunc.EnqueueIn(ctx, 0, tokensToRemove, toDelay)
				}
				return nil
			},
		)
	}

	log.Infof(ctx, "manageFCMTokens(..., %+v, %+v) *** SUCCESS ***", PP(tokensToRemove), PP(tokensToUpdate))

	return nil
}

type FCMData struct {
	DiplicityJSON []byte
}

func NewFCMData(payload interface{}) (*FCMData, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	buf := &bytes.Buffer{}
	w := zlib.NewWriter(buf)
	_, err = w.Write(b)
	if err != nil {
		return nil, err
	}

	w.Close()
	return &FCMData{
		DiplicityJSON: buf.Bytes(),
	}, nil
}

func nestPut(m1 map[string]map[string]string, k1, k2, v string) {
	m2, found := m1[k1]
	if !found {
		m2 = map[string]string{}
	}
	m2[k2] = v
	m1[k1] = m2
}

func fcmSendToTokens(ctx context.Context, lastDelay time.Duration, notif *fcm.NotificationPayload, data *FCMData, tokens map[string][]string) error {
	log.Infof(ctx, "fcmSendToTokens(..., %v, %v, %+v)", PP(notif), PP(data), tokens)

	tokenStrings := []string{}
	userByToken := map[string]string{}
	for uid, userTokens := range tokens {
		for _, tokenString := range userTokens {
			if tokenString != "" {
				tokenStrings = append(tokenStrings, tokenString)
				userByToken[tokenString] = uid
			} else {
				log.Infof(ctx, "Ignoring empty token for %q", uid)
			}
		}
	}

	if len(tokenStrings) == 0 {
		log.Infof(ctx, "No tokens left, exiting")
		return nil
	}

	fcmConf, err := getFCMConf(ctx)
	if err != nil {
		// Safe to retry, nothing got sent.
		log.Errorf(ctx, "Unable to get FCMConf: %v; fix getFCMConf or hope datastore gets fixed", err)
		return err
	}

	client := fcm.NewFcmClient(fcmConf.ServerKey)
	client.SetHTTPClient(urlfetch.Client(ctx))
	client.AppendDevices(tokenStrings)
	if notif != nil {
		client.SetNotificationPayload(notif)
	}
	if data != nil {
		client.SetMsgData(data)
	}

	resp, err := client.Send()
	if err != nil {
		// Safe to retry, nothing got sent probably.
		log.Errorf(ctx, "%v unable to send: %v", PP(client), err)
		return err
	}

	log.Infof(ctx, "Sent %v, received %v, %v in response", PP(client), PP(resp), err)

	if resp.StatusCode == 401 {
		// Safe to retry, we will just keep delaying incrementally until the auth gets fixed.
		msg := fmt.Sprintf("%v unable to send due to 401: %v; fix your authentication", PP(client), PP(resp))
		log.Errorf(ctx, msg)
		return fmt.Errorf(msg)
	}

	if resp.StatusCode == 400 {
		// Can't retry, our payload is fucked up.
		log.Errorf(ctx, "%v unable to send due to 400: %v; unable to recover", PP(client), PP(resp))
		return nil
	}

	idsToRetry := tokens
	if resp.StatusCode > 199 && resp.StatusCode < 299 {
		// Now we have to take care what we retry - retries might lead to duplicates.
		idsToUpdate := map[string]map[string]string{}
		idsToRemove := map[string]map[string]string{}
		idsToRetry = map[string][]string{}

		failures := 0
		successes := 0
		for i, result := range resp.Results {
			token := tokenStrings[i]
			uid := userByToken[token]
			if newID, found := result["registration_id"]; found {
				nestPut(idsToUpdate, uid, token, newID)
			}
			if errMsg, found := result["error"]; found {
				switch errMsg {
				case "InvalidRegistration":
					fallthrough
				case "NotRegistered":
					fallthrough
				case "MismatchSenderId":
					log.Errorf(ctx, "Token %q got %q, will remove it.", token, errMsg)
					nestPut(idsToRemove, uid, token, errMsg)
				case "Unavailable":
					// Can be retried, it's supposed to be.
					fallthrough
				case "InternalServerError":
					// Can be retried, it's supposed to be.
					log.Errorf(ctx, "Token %q got %q, will retry.", token, errMsg)
					idsToRetry[uid] = append(idsToRetry[uid], token)
				case "DeviceMessageRateExceeded":
					fallthrough
				case "TopicsMessageRateExceeded":
					fallthrough
				case "MissingRegistration":
					fallthrough
				case "InvalidTtl":
					fallthrough
				case "InvalidPackageName":
					log.Errorf(ctx, "Token %q got %q, wtf?", token, errMsg)
				case "InvalidParameters":
					fallthrough
				case "MessageTooBig":
					log.Errorf(ctx, "Token %q got %q, SEND SMALLER MESSAGES DAMNIT!", token, errMsg)
				case "InvalidDataKey":
					log.Errorf(ctx, "Token %q got %q, SEND CORRECT MESSAGES DAMNIT!", token, errMsg)
				default:
					log.Errorf(ctx, "Token %q got %q, wtf?", token, errMsg)
				}
				failures++
			} else {
				successes++
			}
		}
		if successes != resp.Success {
			log.Errorf(ctx, "Reported successes %v != nr of non failure results %v", resp.Success, successes)
		}
		if failures != resp.Fail {
			log.Errorf(ctx, "Reported failures %v != nr of failure results %v", resp.Fail, failures)
		}
		if len(idsToRemove) > 0 || len(idsToUpdate) > 0 {
			if err := manageFCMTokensFunc.EnqueueIn(ctx, 0, idsToRemove, idsToUpdate); err != nil {
				log.Errorf(ctx, "Unable to schedule repair of FCM tokens (to remove: %v, to update: %v): %v; hope that datastore gets fixed", PP(idsToRemove), PP(idsToUpdate), err)
			}
		}
	}

	if len(idsToRetry) > 0 {
		// Right, we still have something to retry, but might also have a Retry-After header.
		// First, assume we just double the old delay (or 1 sec).
		delay := lastDelay * 2
		if delay < time.Second {
			delay = time.Second
		}
		// Then, try to honor the Retry-After header.
		if n, err := strconv.ParseInt(resp.RetryAfter, 10, 64); err == nil {
			delay = time.Duration(n) * time.Minute
		} else if at, err := time.Parse(time.RFC1123, resp.RetryAfter); err == nil {
			delay = at.Sub(time.Now())
		}
		// Finally, try to schedule again. If we can't then fuckall we'll try again with the entire payload.
		if err := FCMSendToTokensFunc.EnqueueIn(ctx, delay, delay, notif, data, tokens); err != nil {
			log.Errorf(ctx, "Unable to schedule retry of %v, %v to %+v in %v: %v", PP(notif), PP(data), tokens, delay, err)
			return err
		}
	}

	log.Infof(ctx, "fcmSendToTokens(..., %v, %v, %+v) *** SUCCESS ***", PP(notif), PP(data), tokens)

	return nil
}
