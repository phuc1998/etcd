// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bgentry/speakeasy"
	"go.etcd.io/etcd/pkg/v3/cobrautl"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/mirror"

	"github.com/spf13/cast"
	"github.com/spf13/cobra"
)

var (
	mminsecureTr   bool
	mmcert         string
	mmkey          string
	mmcacert       string
	mmprefix       string
	mmdestprefix   string
	mmuser         string
	mmpassword     string
	mmnodestprefix bool
	mmrev          int64

	modRevision = "__mod_revision"
)

// NewMakeMirrorCommand returns the cobra command for "makeMirror".
func NewMakeMirrorCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "make-mirror [options] <destination>",
		Short: "Makes a mirror at the destination etcd cluster",
		Run:   makeMirrorCommandFunc,
	}

	c.Flags().StringVar(&mmprefix, "prefix", "", "Key-value prefix to mirror")
	c.Flags().Int64Var(&mmrev, "rev", 0, "Specify the kv revision to start to mirror")
	c.Flags().StringVar(&mmdestprefix, "dest-prefix", "", "destination prefix to mirror a prefix to a different prefix in the destination cluster")
	c.Flags().BoolVar(&mmnodestprefix, "no-dest-prefix", false, "mirror key-values to the root of the destination cluster")
	c.Flags().StringVar(&mmcert, "dest-cert", "", "Identify secure client using this TLS certificate file for the destination cluster")
	c.Flags().StringVar(&mmkey, "dest-key", "", "Identify secure client using this TLS key file")
	c.Flags().StringVar(&mmcacert, "dest-cacert", "", "Verify certificates of TLS enabled secure servers using this CA bundle")
	// TODO: secure by default when etcd enables secure gRPC by default.
	c.Flags().BoolVar(&mminsecureTr, "dest-insecure-transport", true, "Disable transport security for client connections")
	c.Flags().StringVar(&mmuser, "dest-user", "", "Destination username[:password] for authentication (prompt if password is not supplied)")
	c.Flags().StringVar(&mmpassword, "dest-password", "", "Destination password for authentication (if this option is used, --user option shouldn't include password)")

	return c
}

func authDestCfg() *clientv3.AuthConfig {
	if mmuser == "" {
		return nil
	}

	var cfg clientv3.AuthConfig

	if mmpassword == "" {
		splitted := strings.SplitN(mmuser, ":", 2)
		if len(splitted) < 2 {
			var err error
			cfg.Username = mmuser
			cfg.Password, err = speakeasy.Ask("Destination Password: ")
			if err != nil {
				cobrautl.ExitWithError(cobrautl.ExitError, err)
			}
		} else {
			cfg.Username = splitted[0]
			cfg.Password = splitted[1]
		}
	} else {
		cfg.Username = mmuser
		cfg.Password = mmpassword
	}

	return &cfg
}

func makeMirrorCommandFunc(cmd *cobra.Command, args []string) {
	if len(args) != 1 {
		cobrautl.ExitWithError(cobrautl.ExitBadArgs, errors.New("make-mirror takes one destination argument"))
	}

	dialTimeout := dialTimeoutFromCmd(cmd)
	keepAliveTime := keepAliveTimeFromCmd(cmd)
	keepAliveTimeout := keepAliveTimeoutFromCmd(cmd)
	sec := &clientv3.SecureConfig{
		Cert:              mmcert,
		Key:               mmkey,
		Cacert:            mmcacert,
		InsecureTransport: mminsecureTr,
	}

	auth := authDestCfg()

	cc := &clientv3.ConfigSpec{
		Endpoints:        []string{args[0]},
		DialTimeout:      dialTimeout,
		KeepAliveTime:    keepAliveTime,
		KeepAliveTimeout: keepAliveTimeout,
		Secure:           sec,
		Auth:             auth,
	}
	dc := mustClient(cc)
	c := mustClientFromCmd(cmd)

	err := makeMirror(context.TODO(), c, dc)
	cobrautl.ExitWithError(cobrautl.ExitError, err)
}

func makeMirror(ctx context.Context, c *clientv3.Client, dc *clientv3.Client) error {
	var (
		revision = int64(0)
		startRev = mmrev - 1
	)

	// if destination prefix is specified and remove destination prefix is true return error
	if mmnodestprefix && len(mmdestprefix) > 0 {
		cobrautl.ExitWithError(cobrautl.ExitBadArgs, errors.New("`--dest-prefix` and `--no-dest-prefix` cannot be set at the same time, choose one"))
	}

	if startRev < 0 { //Ignore if specify the revision
		startRev = 0
		resp, err := c.Get(ctx, modRevision) //Get Mod_Revision to start mirror (We probadly mirror a few keys twice)
		if err == nil && len(resp.Kvs[0].Value) > 0 {
			startRev = cast.ToInt64(string(resp.Kvs[0].Value)) - 1
			revision = startRev
		}
	}

	go func() {
		for {
			//Update Mod_Revision every 1+ second
			time.Sleep(1 * time.Second)
			ct, cl := context.WithTimeout(context.TODO(), 5*time.Second)
			_, err := c.Put(ct, modRevision, cast.ToString(atomic.LoadInt64(&revision)))
			cl()
			if err != nil {
				fmt.Println(err)
			}
		}
	}()

	s := mirror.NewSyncer(c, mmprefix, startRev)

	// If a rev is provided, then do not sync the whole key space.
	// Instead, just start watching the key space starting from the rev
	fmt.Println("startRev " + cast.ToString(startRev))
	if startRev == 0 {
		rc, errc := s.SyncBase(ctx)

		// if remove destination prefix is false and destination prefix is empty set the value of destination prefix same as prefix
		if !mmnodestprefix && len(mmdestprefix) == 0 {
			mmdestprefix = mmprefix
		}

		for r := range rc {
			for _, kv := range r.Kvs {
				_, err := dc.Put(ctx, modifyPrefix(string(kv.Key)), string(kv.Value))
				if err != nil {
					return err
				}
				atomic.StoreInt64(&revision, kv.ModRevision)
			}
		}

		err := <-errc
		if err != nil {
			return err
		}
	}

	wc := s.SyncUpdates(ctx)

	for wr := range wc {
		if wr.CompactRevision != 0 {
			return rpctypes.ErrCompacted
		}

		var lastRev int64
		ops := []clientv3.Op{}

		for _, ev := range wr.Events {
			nextRev := ev.Kv.ModRevision
			if lastRev != 0 && nextRev > lastRev {
				_, err := dc.Txn(ctx).Then(ops...).Commit()
				if err != nil {
					return err
				}
				ops = []clientv3.Op{}
			}
			lastRev = nextRev
			switch ev.Type {
			case mvccpb.PUT:
				ops = append(ops, clientv3.OpPut(modifyPrefix(string(ev.Kv.Key)), string(ev.Kv.Value)))
				atomic.StoreInt64(&revision, ev.Kv.ModRevision)
			case mvccpb.DELETE:
				ops = append(ops, clientv3.OpDelete(modifyPrefix(string(ev.Kv.Key))))
				atomic.StoreInt64(&revision, ev.Kv.ModRevision)
			default:
				panic("unexpected event type")
			}
		}

		if len(ops) != 0 {
			_, err := dc.Txn(ctx).Then(ops...).Commit()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func modifyPrefix(key string) string {
	return strings.Replace(key, mmprefix, mmdestprefix, 1)
}
