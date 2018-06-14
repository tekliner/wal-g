package walg

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/pkg/errors"
	"sync"
	"sort"
)

// walk recursively descends path, calling walkFn.
func walk(path string, info os.FileInfo, walkFn filepath.WalkFunc) error {
	if !info.IsDir() && info.Mode() & os.ModeSymlink == 0 {
		return walkFn(path, info, nil)
	}

	if info.Mode() & os.ModeSymlink != 0 {
		path, _ = filepath.EvalSymlinks(path)
	}

	names, err := readDirNames(path)
	err1 := walkFn(path, info, err)
	// If err != nil, walk can't walk into this directory.
	// err1 != nil means walkFn want walk to skip this directory or stop walking.
	// Therefore, if one of err and err1 isn't nil, walk will return.
	if err != nil || err1 != nil {
		// The caller's behavior is controlled by the return value, which is decided
		// by walkFn. walkFn may ignore err and return nil.
		// If walkFn returns SkipDir, it will be handled by the caller.
		// So walk should return whatever walkFn returns.
		return err1
	}

	for _, name := range names {
		filename := filepath.Join(path, name)
		fileInfo, err := os.Lstat(filename)
		if err != nil {
			if err := walkFn(filename, fileInfo, err); err != nil && err != filepath.SkipDir {
				return err
			}
		} else {
			err = walk(filename, fileInfo, walkFn)
			if err != nil {
				if !fileInfo.IsDir() || err != filepath.SkipDir {
					return err
				}
			}
		}
	}
	return nil
}

// Walk walks the file tree rooted at root, calling walkFn for each file or
// directory in the tree, including root. All errors that arise visiting files
// and directories are filtered by walkFn. The files are walked in lexical
// order, which makes the output deterministic but means that for very
// large directories Walk can be inefficient.
// Walk does not follow symbolic links.
func Walk(root string, walkFn filepath.WalkFunc) error {
	info, err := os.Lstat(root)
	if info.Mode() & os.ModeSymlink != 0 {
		symlinkPath, _ := filepath.EvalSymlinks(root)
		info, err = os.Lstat(symlinkPath)
	}
	if err != nil {
		println(err.Error())
		err = walkFn(root, nil, err)
	} else {
		err = walk(root, info, walkFn)
	}
	if err == filepath.SkipDir {
		return nil
	}
	return err
}

// readDirNames reads the directory named by dirname and returns
// a sorted list of directory entries.
func readDirNames(dirname string) ([]string, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}


// HandleDelete is invoked to perform wal-g delete
func HandleDelete(pre *Prefix, args []string) {
	cfg := ParseDeleteArguments(args, printDeleteUsageAndFail)

	var bk = &Backup{
		Prefix: pre,
		Path:   GetBackupPath(pre),
	}

	if cfg.before {
		if cfg.beforeTime == nil {
			deleteBeforeTarget(cfg.target, bk, pre, cfg.findFull, nil, cfg.dryrun)
		} else {
			backups, err := bk.GetBackups()
			if err != nil {
				log.Fatal(err)
			}
			for _, b := range backups {
				if b.Time.Before(*cfg.beforeTime) {
					deleteBeforeTarget(b.Name, bk, pre, cfg.findFull, backups, cfg.dryrun)
					return
				}
			}
			log.Println("No backups before ", *cfg.beforeTime)
		}
	}
	if cfg.retain {
		number, err := strconv.Atoi(cfg.target)
		if err != nil {
			log.Fatal("Unable to parse number of backups: ", err)
		}
		backups, err := bk.GetBackups()
		if err != nil {
			log.Fatal(err)
		}
		if cfg.full {
			if len(backups) <= number {
				fmt.Printf("Have only %v backups.\n", number)
			}
			left := number
			for _, b := range backups {
				if left == 1 {
					deleteBeforeTarget(b.Name, bk, pre, true, backups, cfg.dryrun)
					return
				}
				dto := fetchSentinel(b.Name, bk, pre)
				if !dto.IsIncremental() {
					left--
				}
			}
			fmt.Printf("Scanned all backups but didn't have %v full.", number)
		} else {
			if len(backups) <= number {
				fmt.Printf("Have only %v backups.\n", number)
			} else {
				cfg.target = backups[number-1].Name
				deleteBeforeTarget(cfg.target, bk, pre, cfg.findFull, nil, cfg.dryrun)
			}
		}
	}
}

// HandleBackupList is invoked to perform wal-g backup-list
func HandleBackupList(pre *Prefix) {
	var bk = &Backup{
		Prefix: pre,
		Path:   GetBackupPath(pre),
	}
	backups, err := bk.GetBackups()
	if err != nil {
		log.Fatal(err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "name\tlast_modified\twal_segment_backup_start")

	for i := len(backups) - 1; i >= 0; i-- {
		b := backups[i]
		fmt.Fprintln(w, fmt.Sprintf("%v\t%v\t%v", b.Name, b.Time.Format(time.RFC3339), b.WalFileName))
	}
}

// HandleBackupFetch is invoked to perform wal-g backup-fetch
func HandleBackupFetch(backupName string, pre *Prefix, dirArc string, mem bool) (lsn *uint64) {
	dirArc = ResolveSymlink(dirArc)
	lsn = deltaFetchRecursion(backupName, pre, dirArc)

	if mem {
		f, err := os.Create("mem.prof")
		if err != nil {
			log.Fatal(err)
		}

		pprof.WriteHeapProfile(f)
		defer f.Close()
	}
	return
}

// deltaFetchRecursion function composes Backup object and recursively searches for necessary base backup
func deltaFetchRecursion(backupName string, pre *Prefix, dirArc string) (lsn *uint64) {
	var bk *Backup
	// Check if BACKUPNAME exists and if it does extract to DIRARC.
	if backupName != "LATEST" {
		bk = &Backup{
			Prefix: pre,
			Path:   GetBackupPath(pre),
			Name:   aws.String(backupName),
		}
		bk.Js = aws.String(*bk.Path + *bk.Name + "_backup_stop_sentinel.json")

		exists, err := bk.CheckExistence()
		if err != nil {
			log.Fatalf("%+v\n", err)
		}
		if !exists {
			log.Fatalf("Backup '%s' does not exist.\n", *bk.Name)
		}

		// Find the LATEST valid backup (checks against JSON file and grabs backup name) and extract to DIRARC.
	} else {
		bk = &Backup{
			Prefix: pre,
			Path:   GetBackupPath(pre),
		}

		latest, err := bk.GetLatest()
		if err != nil {
			log.Fatalf("%+v\n", err)
		}
		bk.Name = aws.String(latest)
	}
	var dto = fetchSentinel(*bk.Name, bk, pre)

	if dto.IsIncremental() {
		fmt.Printf("Delta from %v at LSN %x \n", *dto.IncrementFrom, *dto.IncrementFromLSN)
		deltaFetchRecursion(*dto.IncrementFrom, pre, dirArc)
		fmt.Printf("%v fetched. Upgrading from LSN %x to LSN %x \n", *dto.IncrementFrom, *dto.IncrementFromLSN, dto.LSN)
	}

	unwrapBackup(bk, dirArc, pre, dto)

	lsn = dto.LSN
	return
}

// Do the job of unpacking Backup object
func unwrapBackup(bk *Backup, dirArc string, pre *Prefix, sentinel S3TarBallSentinelDto) {

	incrementBase := path.Join(dirArc, "increment_base")
	if !sentinel.IsIncremental() {
		var empty = true
		searchLambda := func(path string, info os.FileInfo, err error) error {
			if path != dirArc {
				empty = false
			}
			return nil
		}
		filepath.Walk(dirArc, searchLambda)

		if !empty {
			log.Fatalf("Directory %v for delta base must be empty", dirArc)
		}
	} else {
		defer func() {
			err := os.RemoveAll(incrementBase)
			if err != nil {
				log.Fatal(err)
			}
		}()

		err := os.MkdirAll(incrementBase, os.FileMode(0777))
		if err != nil {
			log.Fatal(err)
		}

		files, err := ioutil.ReadDir(dirArc)
		if err != nil {
			log.Fatal(err)
		}

		for _, f := range files {
			objName := f.Name()
			if objName != "increment_base" {
				err := os.Rename(path.Join(dirArc, objName), path.Join(incrementBase, objName))
				if err != nil {
					log.Fatal(err)
				}
			}
		}

		for fileName, fd := range sentinel.Files {
			if !fd.IsSkipped {
				continue
			}
			fmt.Printf("Skipped file %v\n", fileName)
			targetPath := path.Join(dirArc, fileName)
			// this path is only used for increment restoration
			incrementalPath := path.Join(incrementBase, fileName)
			err = MoveFileAndCreateDirs(incrementalPath, targetPath, fileName)
			if err != nil {
				log.Fatal(err, "Failed to move skipped file for "+targetPath+" "+fileName)
			}
		}

	}

	var allKeys []string
	var keys []string
	allKeys, err := bk.GetKeys()
	if err != nil {
		log.Fatalf("%+v\n", err)
	}
	keys = allKeys[:len(allKeys)-1] // TODO: WTF is going on?
	f := &FileTarInterpreter{
		NewDir:             dirArc,
		Sentinel:           sentinel,
		IncrementalBaseDir: incrementBase,
	}
	out := make([]ReaderMaker, len(keys))
	for i, key := range keys {
		s := &S3ReaderMaker{
			Backup:     bk,
			Key:        aws.String(key),
			FileFormat: CheckType(key),
		}
		out[i] = s
	}
	// Extract all compressed tar members except `pg_control.tar.lz4` if WALG version backup.
	err = ExtractAll(f, out)
	if serr, ok := err.(*UnsupportedFileTypeError); ok {
		log.Fatalf("%v\n", serr)
	} else if err != nil {
		log.Fatalf("%+v\n", err)
	}
	// Check name for backwards compatibility. Will check for `pg_control` if WALG version of backup.
	re := regexp.MustCompile(`^([^_]+._{1}[^_]+._{1})`)
	match := re.FindString(*bk.Name)
	if match == "" || sentinel.IsIncremental() {
		// Extract pg_control last. If pg_control does not exist, program exits with error code 1.
		name := *bk.Path + *bk.Name + "/tar_partitions/pg_control.tar.lz4"
		pgControl := &Archive{
			Prefix:  pre,
			Archive: aws.String(name),
		}

		exists, err := pgControl.CheckExistence()
		if err != nil {
			log.Fatalf("%+v\n", err)
		}

		if exists {
			sentinel := make([]ReaderMaker, 1)
			sentinel[0] = &S3ReaderMaker{
				Backup:     bk,
				Key:        aws.String(name),
				FileFormat: CheckType(name),
			}
			err := ExtractAll(f, sentinel)
			if serr, ok := err.(*UnsupportedFileTypeError); ok {
				log.Fatalf("%v\n", serr)
			} else if err != nil {
				log.Fatalf("%+v\n", err)
			}
			fmt.Printf("\nBackup extraction complete.\n")
		} else {
			log.Fatal("Corrupt backup: missing pg_control")
		}
	}
}

func getDeltaConfig() (maxDeltas int, fromFull bool) {
	stepsStr, hasSteps := os.LookupEnv("WALG_DELTA_MAX_STEPS")
	var err error
	if hasSteps {
		maxDeltas, err = strconv.Atoi(stepsStr)
		if err != nil {
			log.Fatal("Unable to parse WALG_DELTA_MAX_STEPS ", err)
		}
	}
	origin, hasOrigin := os.LookupEnv("WALG_DELTA_ORIGIN")
	if hasOrigin {
		switch origin {
		case "LATEST":
		case "LATEST_FULL":
			fromFull = false
		default:
			log.Fatal("Unknown WALG_DELTA_ORIGIN:", origin)
		}
	}
	return
}

// HandleBackupPush is invoked to performa wal-g backup-push
func HandleBackupPush(dirArc string, tu *TarUploader, pre *Prefix) {
	dirArc = ResolveSymlink(dirArc)
	maxDeltas, fromFull := getDeltaConfig()

	var bk = &Backup{
		Prefix: pre,
		Path:   GetBackupPath(pre),
	}

	var dto S3TarBallSentinelDto
	var latest string
	var err error
	incrementCount := 1

	if maxDeltas > 0 {
		latest, err = bk.GetLatest()
		if err != ErrLatestNotFound {
			if err != nil {
				log.Fatalf("%+v\n", err)
			}
			dto = fetchSentinel(latest, bk, pre)
			if dto.IncrementCount != nil {
				incrementCount = *dto.IncrementCount + 1
			}

			if incrementCount > maxDeltas {
				fmt.Println("Reached max delta steps. Doing full backup.")
				dto = S3TarBallSentinelDto{}
			} else if dto.LSN == nil {
				fmt.Println("LATEST backup was made without support for delta feature. Fallback to full backup with LSN marker for future deltas.")
			} else {
				if fromFull {
					fmt.Println("Delta will be made from full backup.")
					latest = *dto.IncrementFullName
					dto = fetchSentinel(latest, bk, pre)
				}
				fmt.Printf("Delta backup from %v with LSN %x. \n", latest, *dto.LSN)
			}
		}
	}

	bundle := &Bundle{
		MinSize:            int64(1000000000), //MINSIZE = 1GB
		IncrementFromLsn:   dto.LSN,
		IncrementFromFiles: dto.Files,
		Files:              &sync.Map{},
	}
	if dto.Files == nil {
		bundle.IncrementFromFiles = make(map[string]BackupFileDescription)
	}

	// Connect to postgres and start/finish a nonexclusive backup.
	conn, err := Connect()
	if err != nil {
		log.Fatalf("%+v\n", err)
	}
	name, lsn, pgVersion, err := bundle.StartBackup(conn, time.Now().String())
	if err != nil {
		log.Fatalf("%+v\n", err)
	}

	if len(latest) > 0 && dto.LSN != nil {
		name = name + "_D_" + stripWalFileName(latest)
	}

	// Start a new tar bundle and walk the DIRARC directory and upload to S3.
	bundle.Tbm = &S3TarBallMaker{
		BaseDir:          filepath.Base(dirArc),
		Trim:             dirArc,
		BkupName:         name,
		Tu:               tu,
		Lsn:              &lsn,
		IncrementFromLsn: dto.LSN,
		IncrementFrom:    latest,
	}

	bundle.StartQueue()
	fmt.Println("Walking ...")
	err = Walk(dirArc, bundle.TarWalker)
	if err != nil {
		log.Fatalf("%+v\n", err)
	}
	err = bundle.FinishQueue()
	if err != nil {
		log.Fatalf("%+v\n", err)
	}
	// Upload `pg_control`.
	err = bundle.HandleSentinel()
	if err != nil {
		log.Fatalf("%+v\n", err)
	}
	// Stops backup and write/upload postgres `backup_label` and `tablespace_map` Files
	finishLsn, err := bundle.HandleLabelFiles(conn)
	if err != nil {
		log.Fatalf("%+v\n", err)
	}

	timelineChanged := bundle.CheckTimelineChanged(conn)
	var sentinel *S3TarBallSentinelDto

	if !timelineChanged {
		sentinel = &S3TarBallSentinelDto{
			LSN:              &lsn,
			IncrementFromLSN: dto.LSN,
			PgVersion:        pgVersion,
		}
		if dto.LSN != nil {
			sentinel.IncrementFrom = &latest
			sentinel.IncrementFullName = &latest
			if dto.IsIncremental() {
				sentinel.IncrementFullName = dto.IncrementFullName
			}
			sentinel.IncrementCount = &incrementCount
		}

		sentinel.SetFiles(bundle.GetFiles())
		sentinel.FinishLSN = &finishLsn
	}

	// Wait for all uploads to finish.
	err = bundle.Tb.Finish(sentinel)
	if err != nil {
		log.Fatalf("%+v\n", err)
	}
}

// HandleWALFetch is invoked to performa wal-g wal-fetch
func HandleWALFetch(pre *Prefix, walFileName string, location string, triggerPrefetch bool) {
	location = ResolveSymlink(location)
	if triggerPrefetch {
		defer forkPrefetch(walFileName, location)
	}

	_, _, running, prefetched := getPrefetchLocations(path.Dir(location), walFileName)
	seenSize := int64(-1)

	for {
		if stat, err := os.Stat(prefetched); err == nil {
			if stat.Size() != int64(WalSegmentSize) {
				log.Println("WAL-G: Prefetch error: wrong file size of prefetched file ", stat.Size())
				break
			}

			err = os.Rename(prefetched, location)
			if err != nil {
				log.Fatalf("%+v\n", err)
			}

			err := checkWALFileMagic(location)
			if err != nil {
				log.Println("Prefetched file contain errors", err)
				os.Remove(location)
				break
			}

			return
		} else if !os.IsNotExist(err) {
			log.Fatalf("%+v\n", err)
		}

		// We have race condition here, if running is renamed here, but it's OK

		if runStat, err := os.Stat(running); err == nil {
			observedSize := runStat.Size() // If there is no progress in 50 ms - start downloading myself
			if observedSize <= seenSize {
				defer func() {
					os.Remove(running) // we try to clean up and ignore here any error
					os.Remove(prefetched)
				}()
				break
			}
			seenSize = observedSize
		} else if os.IsNotExist(err) {
			break // Normal startup path
		} else {
			break // Abnormal path. Permission denied etc. Yes, I know that previous 'else' can be eliminated.
		}
		time.Sleep(50 * time.Millisecond)
	}

	DownloadWALFile(pre, walFileName, location)
}

func checkWALFileMagic(prefetched string) error {
	file, err := os.Open(prefetched)
	if err != nil {
		return err
	}
	defer file.Close()
	magic := make([]byte, 4)
	file.Read(magic)
	if binary.LittleEndian.Uint32(magic) < 0xD061 {
		return errors.New("WAL-G: WAL file magic is invalid ")
	}

	return nil
}

// DownloadWALFile downloads a file and writes it to local file
func DownloadWALFile(pre *Prefix, walFileName string, location string) {
	a := &Archive{
		Prefix:  pre,
		Archive: aws.String(sanitizePath(*pre.Server + "/wal_005/" + walFileName + ".lzo")),
	}
	// Check existence of compressed LZO WAL file
	exists, err := a.CheckExistence()
	if err != nil {
		log.Fatalf("%+v\n", err)
	}
	var crypter = OpenPGPCrypter{}
	if exists {
		arch, err := a.GetArchive()
		if err != nil {
			log.Fatalf("%+v\n", err)
		}

		if crypter.IsUsed() {
			var reader io.Reader
			reader, err = crypter.Decrypt(arch)
			if err != nil {
				log.Fatalf("%v\n", err)
			}
			arch = ReadCascadeClose{reader, arch}
		}

		f, err := os.Create(location)
		if err != nil {
			log.Fatalf("%v\n", err)
		}

		err = DecompressLzo(f, arch)
		if err != nil {
			log.Fatalf("%+v\n", err)
		}
		f.Close()
	} else if !exists {
		// Check existence of compressed LZ4 WAL file
		a.Archive = aws.String(sanitizePath(*pre.Server + "/wal_005/" + walFileName + ".lz4"))
		exists, err = a.CheckExistence()
		if err != nil {
			log.Fatalf("%+v\n", err)
		}

		if exists {
			arch, err := a.GetArchive()
			if err != nil {
				log.Fatalf("%+v\n", err)
			}

			if crypter.IsUsed() {
				var reader io.Reader
				reader, err = crypter.Decrypt(arch)
				if err != nil {
					log.Fatalf("%v\n", err)
				}
				arch = ReadCascadeClose{reader, arch}
			}

			f, err := os.OpenFile(location, os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_EXCL, 0666)
			if err != nil {
				log.Fatalf("%v\n", err)
			}

			size, err := DecompressLz4(f, arch)
			if err != nil {
				log.Fatalf("%+v\n", err)
			}
			if size != int64(WalSegmentSize) {
				log.Fatal("Download WAL error: wrong size ", size)
			}
			err = f.Close()
			if err != nil {
				log.Fatalf("%+v\n", err)
			}
		} else {
			log.Printf("Archive '%s' does not exist.\n", walFileName)
		}
	}
}

// HandleWALPush is invoked to perform wal-g wal-push
func HandleWALPush(tu *TarUploader, dirArc string, pre *Prefix, verify bool) {
	bu := BgUploader{}
	// Look for new WALs while doing main upload
	bu.Start(dirArc, int32(getMaxUploadConcurrency(16)-1), tu, pre, verify)

	UploadWALFile(tu, dirArc, pre, verify)

	bu.Stop()
}

// UploadWALFile from FS to the cloud
func UploadWALFile(tu *TarUploader, dirArc string, pre *Prefix, verify bool) {
	path, err := tu.UploadWal(dirArc, pre, verify)
	if re, ok := err.(Lz4Error); ok {
		log.Fatalf("FATAL: could not upload '%s' due to compression error.\n%+v\n", path, re)
	} else if err != nil {
		log.Printf("upload: could not upload '%s'\n", path)
		log.Fatalf("FATAL%+v\n", err)
	}
}
