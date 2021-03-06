/*
	ZX caching fs
*/
package zxc

import (
	"bytes"
	"clive/dbg"
	"clive/net/auth"
	"clive/u"
	"clive/zx"
	"clive/zx/pred"
	"clive/zx/rzx"
	"clive/zx/zux"
	"errors"
	"fmt"
	"io"
	fpath "path"
	"strings"
	"time"
)

type walkFor int

const (
	forStat     walkFor = iota // walk for stat
	forGet                     // walk for Get()
	forPut                     // walk for Put()
	forDel                     // walk to remove()
	forCreat                   // walk to create a new file/dir
	forLink                    // walk to create a new link
	forCreatAll                // walk to create a new file/dir (and its ancestors)
)

// A caching fs
struct Fs {
	*dbg.Flag
	*zx.Flags
	*zx.Stats
	ai       *auth.Info
	perms    bool
	sync     bool // write-through
	rfs      zx.Getter
	c        fsCache
	syncc    chan bool
	redialc  chan bool
	redialok bool // do we redial?
}

var ctldir = zx.Dir{
	"name":  "Ctl",
	"path":  "/Ctl",
	"addr":  "zxc!/Ctl",
	"mode":  "0644",
	"size":  "0",
	"mtime": "0",
	"type":  "c",
	"uid":   u.Uid,
	"gid":   u.Uid,
	"wuid":  u.Uid,
}

var _fs zx.FullFs = &Fs{}

type ddir zx.Dir

func (d ddir) String() string {
	return zx.Dir(d).LongFmt()
}

func (fs *Fs) String() string {
	return fs.Tag
}

// Return a new view for fs, authenticated for ai
func (fs *Fs) Auth(ai *auth.Info) (zx.Fs, error) {
	nfs := &Fs{}
	*nfs = *fs
	if ai != nil {
		dbg.Warn("%s: auth for %s %v\n", fs.Tag, ai.Uid, ai.Gids)
	}
	nfs.ai = ai
	return nfs, nil
}

func New(rfs zx.Getter) (*Fs, error) {
	rd, err := zx.Stat(rfs, "/")
	if err != nil {
		return nil, err
	}
	tag := fmt.Sprintf("zcx!%s", rfs)
	_, ok := rfs.(redialer)
	fs := &Fs{
		Flag:     &dbg.Flag{Tag: tag},
		Flags:    &zx.Flags{},
		Stats:    &zx.Stats{},
		rfs:      rfs,
		perms:    true,
		syncc:    make(chan bool),
		redialc:  make(chan bool),
		redialok: ok,
	}
	fs.Flags.Add("debug", &fs.Debug)
	fs.Flags.Add("writesync", &fs.sync) // sync after changes
	// TODO: The user u.Uid should be able to change fs.noperms
	fs.Flags.AddRO("perms", &fs.perms)
	fs.Flags.AddRO("redialok", &fs.redialok)
	fs.Flags.Add("clear", func(...string) error {
		fs.Stats.Clear()
		return nil
	})
	fs.Flags.Add("sync", func(...string) error {
		return fs.c.sync(fs.rfs)
	})
	fs.Flags.Add("inval", func(...string) error {
		go fs.c.inval()
		return nil
	})
	if rfs, ok := rfs.(*rzx.Fs); ok {
		fs.Flags.Add("rfsdebug", &rfs.Debug)
		fs.Flags.Add("rfsverb", &rfs.Verb)
	}
	if rfs, ok := rfs.(*zux.Fs); ok {
		fs.Flags.Add("rfsdebug", &rfs.Debug)
	}
	c := &mCache{
		Flag: dbg.Flag{
			Tag: "cache",
		},
	}
	fs.Flags.Add("cachedebug", &c.Debug)
	fs.Flags.Add("verb", &c.Verb)
	fs.Flags.Add("cachestats", &c.stats) // the cache stats all the times
	rd["addr"] = "zxc!/"
	if err := c.setRoot(rd); err != nil {
		return nil, err
	}
	fs.c = c
	go fs.syncer()
	return fs, nil
}

func (fs *Fs) Sync() error {
	err := fs.c.sync(fs.rfs)
	if sfs, ok := fs.rfs.(zx.Syncer); ok {
		if e := sfs.Sync(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

func (fs *Fs) needSync() {
	select {
	case fs.syncc <- true:
	default:
	}
}

func (fs *Fs) needRedial() {
	if !fs.redialok {
		return
	}
	select {
	case fs.redialc <- true:
	default:
	}
}

interface redialer {
	Redial() error
}

func (fs *Fs) redial() error {
	// could place a timeout here
	rfs, ok := fs.rfs.(redialer)
	if !ok {
		dbg.Warn("can't redial: not a redialer")
		return errors.New("not a redialer")
	}
	err := rfs.Redial()
	if err == nil {
		dbg.Warn("%s: reconnected\n", fs.Tag)
	} else {
		fs.Dprintf("redial: %s\n", err)
	}
	return err
}

func (fs *Fs) syncer() {
	ival := syncIval
	last := time.Now()
	redialing := false
	doselect {
	case <-fs.redialc:
		redialing = true
		if err := fs.redial(); err == nil {
			redialing = false
			continue
		}
		ival = 5 * time.Second
	case x := <-fs.syncc:
		if !x {
			break
		}
		if time.Since(last) < syncIval {
			ival = time.Second
			continue
		}
		if redialing {
			if err := fs.redial(); err != nil {
				ival = 5 * time.Second
				continue
			}
			redialing = false
		}
		if err := fs.Sync(); zx.IsIOError(err) && fs.redialok {
			redialing = true
			ival = 5 * time.Second
			continue
		}
		ival = syncIval
		last = time.Now()
	case <-time.After(ival):
		if redialing {
			if err := fs.redial(); err != nil {
				ival = 5 * time.Second
				continue
			}
			redialing = false
		}
		if err := fs.Sync(); zx.IsIOError(err) && fs.redialok {
			redialing = true
			ival = 5 * time.Second
			continue
		}
		ival = syncIval
		last = time.Now()
	}
}

// Syncs and closes both the fs and the underlying fs if it has a close op.
func (fs *Fs) Close() error {
	close(fs.syncc)
	close(fs.redialc)
	err := fs.Sync()
	if xfs, ok := fs.rfs.(io.Closer); ok {
		if e := xfs.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// f must be locked
func (fs *Fs) getMeta(f fsFile) error {
	d, err := zx.Stat(fs.rfs, f.path())
	if err != nil {
		if zx.IsIOError(err) && fs.redialok {
			fs.needRedial()
			return nil // have old meta; use that
		}
		if zx.IsNotExist(err) {
			f.gone()
		}
		return err
	}
	return f.gotMeta(d)
}

// f must be locked
func (fs *Fs) getDirData(f fsFile) error {
	ds, err := zx.GetDir(fs.rfs, f.path())
	if err != nil {
		if zx.IsIOError(err) && fs.redialok && f.oldDataOk() {
			// use the old data
			fs.needRedial()
			return nil
		}
		if zx.IsNotExist(err) {
			f.gone()
		}
		return err
	}
	for _, d := range ds {
		d["addr"] = "zxc!" + d["path"]
	}
	return f.gotDir(ds)
}

// f must be locked
func (fs *Fs) getData(f fsFile) error {
	c := fs.rfs.Get(f.path(), 0, -1)
	err := f.gotData(c)
	if err != nil {
		if zx.IsIOError(err) && fs.redialok && f.oldDataOk() {
			// use the old data
			fs.needRedial()
			return nil
		}
	}
	return err
}

// If the walk works, f is returned locked
func (fs *Fs) walk(why walkFor, nd zx.Dir, els ...string) (f fsFile, err error) {
	f = fs.c.root()
	for {
		fs.Dprintf("walk %s...\n", f)
		f.Lock()
		if !f.metaOk() {
			if err := fs.getMeta(f); err != nil {
				defer f.Unlock()
				return f, fmt.Errorf("%s: %s", f, err)
			}
		}
		if f.isDel() {
			defer f.Unlock()
			return f, fmt.Errorf("%s: %s", f, zx.ErrNotExist)
		}
		d := f.dir()
		if len(els) == 0 {
			switch why {
			case forStat:
			case forGet:
				if fs.perms && !d.CanGet(fs.ai) {
					defer f.Unlock()
					return f, fmt.Errorf("%s: %s", f, zx.ErrPerm)
				}
				if !f.dataOk() {
					if d["type"] == "d" {
						if err := fs.getDirData(f); err != nil {
							defer f.Unlock()
							return f, fmt.Errorf("%s: %s", f, err)
						}
					} else {
						if err := fs.getData(f); err != nil {
							defer f.Unlock()
							return f, fmt.Errorf("%s: %s", f, err)
						}
					}
				}
			case forPut:
				if d["type"] == "d" {
					defer f.Unlock()
					return f, fmt.Errorf("%s: %s", f, zx.ErrIsDir)
				}
				if !f.dataOk() {
					if err := fs.getData(f); err != nil {
						defer f.Unlock()
						return f, fmt.Errorf("%s: %s", f, err)
					}
				}
			case forDel:
				if !f.dataOk() && d["type"] == "d" {
					if err := fs.getDirData(f); err != nil {
						defer f.Unlock()
						return f, fmt.Errorf("%s: %s", f, err)
					}
				}
			case forCreat, forCreatAll:
				if false && d["type"] == "d" {
					defer f.Unlock()
					return f, fmt.Errorf("%s: %s", f, zx.ErrExists)
				}
			case forLink:
				defer f.Unlock()
				return f, fmt.Errorf("%s: %s", f, zx.ErrExists)
			}
			return f, nil
		}
		if d["type"] != "d" {
			defer f.Unlock()
			return f, fmt.Errorf("%s: %s", f, zx.ErrNotDir)
		}
		if fs.perms && !d.CanWalk(fs.ai) {
			defer f.Unlock()
			return f, fmt.Errorf("%s: %s", f, zx.ErrPerm)
		}
		if !f.dataOk() {
			if err := fs.getDirData(f); err != nil {
				defer f.Unlock()
				return f, fmt.Errorf("%s: %s", f, err)
			}
		}
		if len(els) == 1 {
			switch why {
			case forStat:
				if fs.perms && !d.CanGet(fs.ai) {
					defer f.Unlock()
					return f, fmt.Errorf("%s: %s", f, zx.ErrPerm)
				}
			case forDel, forCreat, forCreatAll, forLink:
				if fs.perms && !d.CanPut(fs.ai) {
					defer f.Unlock()
					return f, fmt.Errorf("%s: %s", f, zx.ErrPerm)
				}
			}
		}
		cf, err := f.walk1(els[0])
		isnotexist := zx.IsNotExist(err)
		if why == forCreatAll && isnotexist && len(els) > 1 {
			dd := nd.Dup()
			od := f.dir()
			dd["type"] = "d"
			dd["name"] = els[0]
			dd["path"] = fpath.Join(od["path"], els[0])
			dd["addr"] = "zxc!" + dd["path"]
			uid := od["uid"]
			gid := od["gid"]
			if fs.ai != nil {
				uid = fs.ai.Uid
			}
			dd["uid"] = uid
			dd["gid"] = gid
			dd["mode"] = od["mode"]
			dd.SetTime("mtime", time.Now())
			dd["wuid"] = uid
			nf, nerr := f.newFile(dd, fs.rfs)
			if nerr == nil {
				err = nil
				cf = nf
			}
		}
		if err != nil {
			if (why == forCreat || why == forCreatAll || why == forLink) &&
				len(els) == 1 && isnotexist {
				return f, nil
			}
			defer f.Unlock()
			return f, fmt.Errorf("%s: %s: %s", f, els[0], err)
		}
		f.Unlock()
		f = cf
		els = els[1:]
	}
}

func (fs *Fs) stat(p string) (zx.Dir, error) {
	p, err := zx.UseAbsPath(p)
	if err != nil {
		return nil, err
	}
	if p == "/Ctl" {
		return ctldir.Dup(), nil
	}
	f, err := fs.walk(forStat, nil, zx.Elems(p)...)
	if err != nil {
		return nil, err
	}
	defer f.Unlock()
	return f.dir().Dup(), nil
}

func (fs *Fs) Stat(p string) <-chan zx.Dir {
	fs.Count(zx.Sstat)
	c := make(chan zx.Dir, 1)
	d, err := fs.stat(p)
	if err == nil {
		fs.Dprintf("stat %s: %s\n", p, ddir(d))
		c <- d
	} else {
		fs.Dprintf("stat %s: %s\n", p, err)
	}
	close(c, err)
	return c
}

func (fs *Fs) wstat(p string, nd zx.Dir) (zx.Dir, error) {
	p, err := zx.UseAbsPath(p)
	if err != nil {
		return nil, err
	}
	if p == "/Ctl" {
		// wstat is ignored for this file
		return ctldir.Dup(), nil
	}
	why := forStat
	if nd["size"] != "" {
		why = forPut
	}
	f, err := fs.walk(why, nil, zx.Elems(p)...)
	if err != nil {
		return nil, err
	}
	d := f.dir()
	ai := fs.ai
	if !fs.perms {
		ai = nil
	}
	if d["type"] == "d" {
		delete(nd, "size")
	}
	if nd["wuid"] != "" {
		delete(nd, "wuid")
	}
	if err := d.CanWstat(ai, nd); err != nil {
		f.Unlock()
		return nil, fmt.Errorf("%s: %s", p, err)
	}
	if err := f.wstat(nd); err != nil {
		f.Unlock()
		return nil, err
	}
	d = d.Dup()
	f.Unlock()
	if fs.sync {
		f.sync(fs.rfs)
	} else {
		fs.needSync()
	}
	return d, nil
}

func (fs *Fs) Wstat(p string, nd zx.Dir) <-chan zx.Dir {
	fs.Count(zx.Swstat)
	c := make(chan zx.Dir, 1)
	nd = nd.SysDup()
	d, err := fs.wstat(p, nd)
	if err == nil {
		fs.Dprintf("wstat %s: %s\n\t-> %s\n", p, nd, ddir(d))
		c <- d
	} else {
		fs.Dprintf("wstat %s: %s\n", p, err)
	}
	close(c, err)
	return c
}

func (fs *Fs) getCtl(off, count int64, dc chan<- []byte) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "lfs %s:\n", fs.Tag)
	fmt.Fprintf(&buf, "%s", fs.Flags)
	fmt.Fprintf(&buf, "%s", fs.Stats)
	rctl, err := zx.GetAll(fs.rfs, "/Ctl")
	if err == nil {
		buf.Write(rctl)
	}
	resp := buf.Bytes()
	o := int(off)
	if o >= len(resp) {
		o = len(resp)
	}

	resp = resp[o:]
	n := int(count)
	if n > len(resp) || n < 0 {
		n = len(resp)
	}
	if ok := dc <- resp[:n]; !ok {
		return cerror(dc)
	}
	return nil
}

func (fs *Fs) get(p string, off, count int64, c chan<- []byte) error {
	p, err := zx.UseAbsPath(p)
	if err != nil {
		return err
	}
	if p == "/Ctl" {
		return fs.getCtl(off, count, c)
	}
	f, err := fs.walk(forGet, nil, zx.Elems(p)...)
	if err != nil {
		return err
	}
	d := f.dir()
	if d["type"] != "d" {
		// this unlocks f before actually sending anything
		return f.getData(off, count, c)
	}
	ds, err := f.getDir()
	f.Unlock()
	if err != nil {
		return err
	}
	ctlsent := false
Dloop:
	for i := 0; i < len(ds); {
		if off > 0 {
			off--
			if !ctlsent && p == "/" {
				ctlsent = true
			} else {
				i++
			}
			continue
		}
		switch count {
		case zx.All:
			break
		case 0:
			break Dloop
		default:
			count--
		}
		if !ctlsent && p == "/" {
			ctlsent = true
			if ok := c <- ctldir.Bytes(); !ok {
				return cerror(c)
			}
			// but not i++
			continue
		}
		d := ds[i]
		if d["name"] == ".zx" || d["name"] == ".#zx" { // .#zx was the old AttrFile
			if i == len(ds)-1 {
				break
			}
			copy(ds[i:], ds[i+1:])
			ds = ds[:len(ds)-1]
			continue
		}
		if ok := c <- d.Bytes(); !ok {
			return cerror(c)
		}
		i++
	}
	return nil
}

func (fs *Fs) Get(p string, off, count int64) <-chan []byte {
	fs.Dprintf("get %s %d %d...\n", p, off, count)
	c := make(chan []byte)
	go func() {
		fs.Count(zx.Sget)
		err := fs.get(p, off, count, c)
		if err != nil {
			fs.Dprintf("get %s: %s\n", p, err)
		}
		close(c, err)
	}()
	return c
}

func (fs *Fs) remove(p string, all bool) error {
	fs.Count(zx.Sremove)
	p, err := zx.UseAbsPath(p)
	if err != nil {
		return err
	}
	if p == "/Ctl" || p == "/" {
		return fmt.Errorf("remove %s: %s", p, zx.ErrPerm)
	}
	els := zx.Elems(p)
	f, err := fs.walk(forDel, nil, els...)
	if err != nil {
		return err
	}
	err = f.remove(all)
	f.Unlock()
	if fs.sync {
		f.sync(fs.rfs)
	} else {
		fs.needSync()
	}

	return err
}

func (fs *Fs) Remove(p string) <-chan error {
	fs.Dprintf("remove %s...\n", p)
	c := make(chan error, 1)
	err := fs.remove(p, false)
	if err != nil {
		fs.Dprintf("remove %s: %s\n", p, err)
	}
	c <- err
	close(c, err)
	return c
}

func (fs *Fs) RemoveAll(p string) <-chan error {
	fs.Dprintf("removeall %s...\n", p)
	c := make(chan error, 1)
	err := fs.remove(p, true)
	if err != nil {
		fs.Dprintf("removeall %s: %s\n", p, err)
	}
	c <- err
	close(c, err)
	return c
}

func inconsistentMove(from, to string) bool {
	if from == to {
		return false
	}
	// moves from inside itself?
	// i.e. is from a prefix of to
	return zx.HasPrefix(to, from)
}

func (fs *Fs) move(from, to string) error {
	rfs, ok := fs.rfs.(zx.Mover)
	if !ok {
		return fmt.Errorf("%s: move not supported", fs.Tag)
	}
	from, err := zx.UseAbsPath(from)
	if err != nil {
		return err
	}
	to, err = zx.UseAbsPath(to)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if from == "/Ctl" || from == "/" {
		return fmt.Errorf("move %s: %s", from, zx.ErrPerm)
	}
	if to == "/Ctl" || to == "/" {
		return fmt.Errorf("move %s: %s", to, zx.ErrPerm)
	}
	if inconsistentMove(from, to) {
		return fmt.Errorf("move %s: inconsistent move", from)
	}
	fs.c.sync(fs.rfs)
	fromels := zx.Elems(from)
	ffrom, err := fs.walk(forDel, nil, fromels...)
	if err != nil {
		return err
	}
	ffrom.inval()
	ffrom.Unlock()
	toels := zx.Elems(to)
	fto, err := fs.walk(forCreat, nil, toels...)
	if err != nil {
		return err
	}
	fto.inval()
	fto.Unlock()
	// now we have a race,
	// lock the parents, invalidate them,
	// issue the request to the remote fs and we are done.
	if err != nil {
		return err
	}
	pfrom := fpath.Dir(from)
	pto := fpath.Dir(to)
	// The lock order must be this way,
	// becase walk returns the file locked, and we must walk
	// the inner path first, if one is a prefix of another
	switch {
	case pfrom > pto:
		ffrom, err = fs.walk(forStat, nil, fromels[:len(fromels)-1]...)
		if err != nil {
			return err
		}
		defer ffrom.Unlock()
		fto, err = fs.walk(forStat, nil, toels[:len(toels)-1]...)
		if err != nil {
			return err
		}
		defer fto.Unlock()
	case pfrom == pto:
		ffrom, err = fs.walk(forStat, nil, fromels[:len(fromels)-1]...)
		if err != nil {
			return err
		}
		defer ffrom.Unlock()
	case pfrom < pto:
		fto, err = fs.walk(forStat, nil, toels[:len(toels)-1]...)
		if err != nil {
			return err
		}
		defer fto.Unlock()
		ffrom, err = fs.walk(forStat, nil, fromels[:len(fromels)-1]...)
		if err != nil {
			return err
		}
		defer ffrom.Unlock()
	}
	ffrom.inval()
	fto.inval()
	if err := <-rfs.Move(from, to); err != nil {
		return err
	}
	return nil
}

func (fs *Fs) Move(from, to string) <-chan error {
	fs.Dprintf("move %s %s...\n", from, to)
	c := make(chan error, 1)
	fs.Count(zx.Smove)
	err := fs.move(from, to)
	if err != nil {
		fs.Dprintf("move %s: %s\n", from, err)
	}
	c <- err
	close(c, err)
	return c
}

func inconsistentLink(oldp, newp string) bool {
	// links back to a parent?
	// i.e. is oldp a prefix of newp
	return zx.HasPrefix(newp, oldp)
}

// The final server might support link, but we do not.
// Instead, we just forward the call and our cache will just see
// more files than it would see if links did not exist.
func (fs *Fs) link(to, from string) error {
	rfs, ok := fs.rfs.(zx.Linker)
	if !ok {
		return fmt.Errorf("%s: link not supported", fs.Tag)
	}
	from, err := zx.UseAbsPath(from)
	if err != nil {
		return err
	}
	to, err = zx.UseAbsPath(to)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if from == "/Ctl" || from == "/" {
		return fmt.Errorf("link %s: %s", from, zx.ErrPerm)
	}
	if to == "/Ctl" || to == "/" {
		return fmt.Errorf("link %s: %s", to, zx.ErrPerm)
	}
	if inconsistentLink(from, to) {
		return fmt.Errorf("link %s: inconsistent link", from)
	}
	fs.c.sync(fs.rfs)
	toels := zx.Elems(from)
	fto, err := fs.walk(forStat, nil, toels...)
	if err != nil {
		return err
	}
	fto.Unlock()
	fromels := zx.Elems(from)
	ffrom, err := fs.walk(forLink, nil, fromels...)
	if err != nil {
		return err
	}
	defer ffrom.Unlock()
	ffrom.inval()
	err = <-rfs.Link(to, from)
	fs.getDirData(ffrom)
	return err
}

func (fs *Fs) Link(oldp, newp string) <-chan error {
	fs.Dprintf("link %s %s...\n", oldp, newp)
	c := make(chan error, 1)
	fs.Count(zx.Slink)
	err := fs.link(oldp, newp)
	if err != nil {
		fs.Dprintf("link %s: %s\n", oldp, err)
	}
	c <- err
	close(c, err)
	return c
}

func (fs *Fs) putCtl(c <-chan []byte) error {
	var buf bytes.Buffer
	for d := range c {
		buf.Write(d)
	}
	if err := cerror(c); err != nil {
		return err
	}
	ctl := buf.String()
	if strings.HasPrefix(ctl, "pass ") {
		rfs, ok := fs.rfs.(zx.Putter)
		if !ok {
			return errors.New("can't pass ctl: rfs is not a putter")
		}
		passc := make(chan []byte, 1)
		passc <- []byte(ctl[5:])
		close(passc)
		rc := rfs.Put("/Ctl", nil, 0, passc)
		<-rc
		return cerror(rc)
	}
	return fs.Ctl(ctl)

}

func (fs *Fs) put(p string, d zx.Dir, off int64, c <-chan []byte) (zx.Dir, error) {
	p, err := zx.UseAbsPath(p)
	if err != nil {
		return nil, err
	}
	if p == "/" {
		return nil, fmt.Errorf("/: %s", zx.ErrIsDir)
	}
	if p == "/Ctl" {
		return ctldir.Dup(), fs.putCtl(c)
	}
	els := zx.Elems(p)
	var f fsFile
	typ := d["type"]
	switch typ {
	case "":
		f, err = fs.walk(forPut, nil, els...)
	case "d", "-":
		if typ == "d" {
			delete(d, "size")
		} else if d["size"] == "" {
			d["size"] = "0"
		}
		f, err = fs.walk(forCreat, nil, els...)
	case "D":
		if typ == "d" {
			delete(d, "size")
		}
		d["type"] = "d"
		typ = "d"
		f, err = fs.walk(forCreatAll, d, els...)
	case "F":
		d["type"] = "-"
		typ = "-"
		if d["size"] == "" {
			d["size"] = "0"
		}
		f, err = fs.walk(forCreatAll, d, els...)
	default:
		return nil, fmt.Errorf("%s: bad file type '%s'", p, typ)
	}
	if err != nil {
		return nil, err
	}

	wd := f.dir()
	if wd["path"] == p && typ != "" && wd["type"] != typ {
		defer f.Unlock()
		return nil, fmt.Errorf("%s: '%s'", p, zx.ErrExists)
	}
	uid := wd["uid"]
	gid := wd["gid"]
	if uid == "" {
		uid = u.Uid
		if fs.ai != nil && fs.ai.Uid != "" {
			uid = fs.ai.Uid
		}
	}
	if gid == "" {
		gid = uid
	}
	if wd["path"] != p {
		wd = zx.Dir{"type": typ, "mode": wd["mode"], "uid": uid, "gid": gid}
	}
	var ai *auth.Info
	if fs.perms {
		ai = fs.ai
	}
	if err := wd.CanWstat(ai, d); err != nil {
		f.Unlock()
		return nil, err
	}
	umtime := d["mtime"]
	if umtime == "" {
		d.SetTime("mtime", time.Now())
	}
	d["wuid"] = uid
	if wd["path"] != p { // new dir or new file
		d["type"] = typ
		if d["uid"] == "" {
			d["uid"] = uid
		}
		if d["gid"] == "" {
			d["gid"] = gid
		}
		d["name"] = fpath.Base(p)
		d["path"] = p
		if d["mode"] == "" {
			d["mode"] = wd["mode"]
		}
		if d["size"] == "" {
			d["size"] = "0"
		}
		d["addr"] = "zxc!" + p
		nf, err := f.newFile(d, fs.rfs)
		f.Unlock()
		if err != nil {
			return nil, err
		}
		if typ == "d" {
			return d, nil
		}
		f = nf
		f.Lock()
	} else if typ == "-" {
		f.wstat(zx.Dir{"size": "0"})
	}
	if c == nil {
		c = make(chan []byte)
		close(c)
	}
	if err := f.wstat(d); err != nil {
		f.Unlock()
		return nil, err
	}
	if typ == "d" {
		d := f.dir().Dup()
		f.Unlock()
		if fs.sync {
			f.sync(fs.rfs)
		} else {
			fs.needSync()
		}
		return d, nil
	}
	// putData will unlock f
	err = f.putData(off, c, umtime)
	f.Lock()
	d = f.dir().Dup()
	f.Unlock()
	if fs.sync {
		f.sync(fs.rfs)
	} else {
		fs.needSync()
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (fs *Fs) Put(p string, d zx.Dir, off int64, c <-chan []byte) <-chan zx.Dir {
	fs.Dprintf("put %s %d %s...\n", p, off, ddir(d))
	rc := make(chan zx.Dir)
	go func() {
		fs.Count(zx.Sput)
		d = d.SysDup()
		d, err := fs.put(p, d, off, c)
		if err == nil {
			rc <- d
		} else {
			fs.Dprintf("put %s: %s\n", p, err)
			close(c, err)
		}
		close(rc, err)
	}()
	return rc
}

// f is locked and will be unlocked before findr returns
// its data is valid at the time of the call
func (fs *Fs) findr(f fsFile, d zx.Dir, fp *pred.Pred, p, spref, dpref string, lvl int, c chan<- zx.Dir) error {
	match, pruned, err := fp.EvalAt(d, lvl)
	fs.Dprintf("findr at %v\n\t%v\n\t%v %v %v\n\n", d.LongFmt(), p, match, pruned, err)
	if pruned {
		f.Unlock()
		if !match {
			d["err"] = "pruned"
			fs.Dprintf("find <-! %s\n", ddir(d))
		} else {
			fs.Dprintf("find <- %s\n", ddir(d))
		}
		c <- d
		return nil
	}
	if err != nil {
		f.Unlock()
		return err
	}
	if d["rm"] != "" {
		f.Unlock()
		return nil
	}
	var ds []zx.Dir
	if d["type"] == "d" {
		ds, err = f.getDir()
		if err != nil {
			d["err"] = err.Error()
		} else if f.path() == "/" {
			nds := []zx.Dir{ctldir.Dup()}
			nds = append(nds, ds...)
			ds = nds
		}
	}
	f.Unlock()
	if match || err != nil {
		fs.Dprintf("find <- %s\n", ddir(d))
		if ok := c <- d; !ok {
			return cerror(c)
		}
	}

	for i := 0; i < len(ds); i++ {
		cd := ds[i]
		f.Lock()
		var cf fsFile
		if cd["path"] == "/Ctl" {
			cf = ctlfile
		} else {
			cf, err = f.walk1(cd["name"])
		}
		f.Unlock()
		if err != nil || cd["rm"] != "" {
			continue
		}
		cp := cd["path"]
		if spref != dpref {
			cpath := cd["path"]
			suff := zx.Suffix(cpath, spref)
			if suff == "" {
				return fmt.Errorf("Y%s: %s: %s", spref, cpath, zx.ErrNotSuffix)
			}
			cd["path"] = fpath.Join(dpref, suff)
		}
		cf.Lock()
		if cd["type"] == "d" && !cf.dataOk() {
			if err := fs.getDirData(cf); err != nil {
				defer cf.Unlock()
				return fmt.Errorf("%s: %s", cf, err)
			}
		}
		// findr will unlock cf
		if err := fs.findr(cf, cd, fp, cp, spref, dpref, lvl+1, c); err != nil {
			return err
		}
	}
	return nil
}

func (fs *Fs) find(p, fpred, spref, dpref string, depth int, c chan<- zx.Dir) error {
	p, err := zx.UseAbsPath(p)
	if err != nil {
		return err
	}
	var f fsFile
	var d zx.Dir
	if p == "/Ctl" {
		f = ctlfile
		ctlfile.Lock()
		d = ctldir.Dup()
	} else {
		f, err = fs.walk(forGet, nil, zx.Elems(p)...)
		if err != nil {
			return err
		}
		d = f.dir().Dup()
	}
	if spref != "" || dpref != "" {
		spref, err = zx.UseAbsPath(spref)
		if err != nil {
			f.Unlock()
			return err
		}
		dpref, err = zx.UseAbsPath(dpref)
		if err != nil {
			f.Unlock()
			return err
		}
	}
	fp, err := pred.New(fpred)
	if err != nil {
		f.Unlock()
		return err
	}
	if spref != dpref {
		suff := zx.Suffix(p, spref)
		if suff == "" {
			f.Unlock()
			return fmt.Errorf("suffix %s %s: %s", spref, p, zx.ErrNotSuffix)
		}
		d["path"] = fpath.Join(dpref, suff)
	}
	// findr unlocks f
	return fs.findr(f, d, fp, p, spref, dpref, depth, c)
}

func (fs *Fs) Find(path, fpred, spref, dpref string, depth0 int) <-chan zx.Dir {
	fs.Dprintf("find %s %q %q %d...\n", path, spref, dpref, depth0)
	c := make(chan zx.Dir)
	go func() {
		fs.Count(zx.Sfind)
		err := fs.find(path, fpred, spref, dpref, depth0, c)
		if err != nil {
			fs.Dprintf("find %s: %s\n", path, err)
		}
		close(c, err)
	}()
	return c
}

func (fs *Fs) dpath(d zx.Dir) string {
	old := d["addr"]
	p := strings.LastIndexByte(old, '!')
	if p < 0 {
		p = 0
	} else {
		p++
	}
	return old[p:]
}

func (fs *Fs) FindGet(path, fpred, spref, dpref string, depth0 int) <-chan face{} {
	fs.Dprintf("findget %s %q %q %d...\n", path, spref, dpref, depth0)
	c := make(chan face{})
	go func() {
		dc := fs.Find(path, fpred, spref, dpref, depth0)
		for d := range dc {
			if ok := c <- d.Dup(); !ok {
				close(dc, cerror(c))
				return
			}
			if d["err"] != "" || d["type"] == "d" {
				continue
			}
			p := fs.dpath(d)
			if p == "" {
				panic("zux: bad dir addr path")
			}
			bc := fs.Get(p, 0, -1)
			for d := range bc {
				c <- d
			}
			if err := cerror(bc); err != nil {
				c <- err
			}
		}
		close(c, cerror(dc))
	}()
	return c
}
