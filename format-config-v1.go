/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// fsFormat - structure holding 'fs' format.
type fsFormat struct {
	Version string `json:"version"`
}

// xlFormat - structure holding 'xl' format.
type xlFormat struct {
	Version string `json:"version"` // Version of 'xl' format.
	Disk    string `json:"disk"`    // Disk field carries assigned disk uuid.
	// JBOD field carries the input disk order generated the first
	// time when fresh disks were supplied.
	JBOD []string `json:"jbod"`
}

// formatConfigV1 - structure holds format config version '1'.
type formatConfigV1 struct {
	Version string `json:"version"` // Version of the format config.
	// Format indicates the backend format type, supports two values 'xl' and 'fs'.
	Format string    `json:"format"`
	FS     *fsFormat `json:"fs,omitempty"` // FS field holds fs format.
	XL     *xlFormat `json:"xl,omitempty"` // XL field holds xl format.
}

/*

All disks online
-----------------
- All Unformatted - format all and return success.
- Some Unformatted - format all and return success.
- Any JBOD inconsistent - return failure // Requires deep inspection, phase2.
- Some are corrupt (missing format.json) - return failure  // Requires deep inspection, phase2.
- Any unrecognized disks - return failure

Some disks are offline and we have quorum.
-----------------
- Some unformatted - no heal, return success.
- Any JBOD inconsistent - return failure // Requires deep inspection, phase2.
- Some are corrupt (missing format.json) - return failure  // Requires deep inspection, phase2.
- Any unrecognized disks - return failure

No read quorum
-----------------
failure for all cases.

// Pseudo code for managing `format.json`.

// Generic checks.
if (no quorum) return error
if (any disk is corrupt) return error // phase2
if (jbod inconsistent) return error // phase2
if (disks not recognized) // Always error.

// Specific checks.
if (all disks online)
  if (all disks return format.json)
     if (jbod consistent)
        if (all disks recognized)
          return
  else
     if (all disks return format.json not found)
        (initialize format)
        return
     else (some disks return format.json not found)
        (heal format)
        return
     fi
   fi
else // No healing at this point forward, some disks are offline or dead.
   if (some disks return format.json not found)
      if (with force)
         // Offline disks are marked as dead.
         (heal format) // Offline disks should be marked as dead.
         return success
      else (without force)
         // --force is necessary to heal few drives, because some drives
         // are offline. Offline disks will be marked as dead.
         return error
      fi
fi
*/

// error returned when some disks are found to be unformatted.
var errSomeDiskUnformatted = errors.New("some disks are found to be unformatted")

// error returned when some disks are offline.
var errSomeDiskOffline = errors.New("some disks are offline")

// errDiskOrderMismatch - returned when disk UUID is not in consistent JBOD order.
var errDiskOrderMismatch = errors.New("disk order mismatch")

// Returns error slice into understandable errors.
func reduceFormatErrs(errs []error, diskCount int) error {
	var errUnformattedDiskCount = 0
	var errDiskNotFoundCount = 0
	for _, err := range errs {
		if err == errUnformattedDisk {
			errUnformattedDiskCount++
		} else if err == errDiskNotFound {
			errDiskNotFoundCount++
		}
	}
	// Returns errUnformattedDisk if all disks report unFormattedDisk.
	if errUnformattedDiskCount == diskCount {
		return errUnformattedDisk
	} else if errUnformattedDiskCount < diskCount && errDiskNotFoundCount == 0 {
		// Only some disks return unFormattedDisk and all disks are online.
		return errSomeDiskUnformatted
	} else if errUnformattedDiskCount < diskCount && errDiskNotFoundCount > 0 {
		// Only some disks return unFormattedDisk and some disks are
		// offline as well.
		return errSomeDiskOffline
	}
	return nil
}

// loadAllFormats - load all format config from all input disks in parallel.
func loadAllFormats(bootstrapDisks []StorageAPI) ([]*formatConfigV1, []error) {
	// Initialize sync waitgroup.
	var wg = &sync.WaitGroup{}

	// Initialize list of errors.
	var sErrs = make([]error, len(bootstrapDisks))

	// Initialize format configs.
	var formatConfigs = make([]*formatConfigV1, len(bootstrapDisks))

	// Make a volume entry on all underlying storage disks.
	for index, disk := range bootstrapDisks {
		wg.Add(1)
		// Make a volume inside a go-routine.
		go func(index int, disk StorageAPI) {
			defer wg.Done()
			formatConfig, lErr := loadFormat(disk)
			if lErr != nil {
				sErrs[index] = lErr
				return
			}
			formatConfigs[index] = formatConfig
		}(index, disk)
	}

	// Wait for all make vol to finish.
	wg.Wait()

	for _, err := range sErrs {
		if err != nil {
			// Return all formats and errors.
			return formatConfigs, sErrs
		}
	}
	// Return all formats and nil
	return formatConfigs, nil
}

// genericFormatCheck - validates and returns error.
// if (no quorum) return error
// if (any disk is corrupt) return error // phase2
// if (jbod inconsistent) return error // phase2
// if (disks not recognized) // Always error.
func genericFormatCheck(formatConfigs []*formatConfigV1, sErrs []error) (err error) {
	// Calculate the errors.
	var (
		errCorruptFormatCount = 0
		errCount              = 0
	)

	// Through all errors calculate the actual errors.
	for _, lErr := range sErrs {
		if lErr == nil {
			continue
		}
		// These errors are good conditions, means disk is online.
		if lErr == errUnformattedDisk || lErr == errVolumeNotFound {
			continue
		}
		if lErr == errCorruptedFormat {
			errCorruptFormatCount++
		} else {
			errCount++
		}
	}

	// Calculate read quorum.
	readQuorum := len(formatConfigs)/2 + 1

	// Validate the err count under tolerant limit.
	if errCount > len(formatConfigs)-readQuorum {
		return errXLReadQuorum
	}

	// One of the disk has corrupt format, return error.
	if errCorruptFormatCount > 0 {
		return errCorruptedFormat
	}

	// Validates if format and JBOD are consistent across all disks.
	if err = checkFormatXL(formatConfigs); err != nil {
		return err
	}

	// Success..
	return nil
}

// isSavedUUIDInOrder - validates if disk uuid is present and valid in all
// available format config JBOD. This function also validates if the disk UUID
// is always available on all JBOD under the same order.
func isSavedUUIDInOrder(uuid string, formatConfigs []*formatConfigV1) bool {
	var orderIndexes []int
	// Validate each for format.json for relevant uuid.
	for _, formatConfig := range formatConfigs {
		if formatConfig == nil {
			continue
		}
		// Validate if UUID is present in JBOD.
		uuidIndex := findDiskIndex(uuid, formatConfig.XL.JBOD)
		if uuidIndex == -1 {
			// UUID not found.
			errorIf(errDiskNotFound, "Disk %s not found in JBOD list", uuid)
			return false
		}
		// Save the position of UUID present in JBOD.
		orderIndexes = append(orderIndexes, uuidIndex+1)
	}
	// Once uuid is found, verify if the uuid
	// present in same order across all format configs.
	prevOrderIndex := orderIndexes[0]
	for _, orderIndex := range orderIndexes {
		if prevOrderIndex != orderIndex {
			errorIf(errDiskOrderMismatch, "Disk %s is in wrong order wanted %d, saw %d ", uuid, prevOrderIndex, orderIndex)
			return false
		}
	}
	// Returns success, when we have verified if uuid
	// is consistent and valid across all format configs.
	return true
}

// checkDisksConsistency - checks if all disks are consistent with all JBOD entries on all disks.
func checkDisksConsistency(formatConfigs []*formatConfigV1) error {
	var disks = make([]string, len(formatConfigs))
	// Collect currently available disk uuids.
	for index, formatConfig := range formatConfigs {
		if formatConfig == nil {
			disks[index] = ""
			continue
		}
		disks[index] = formatConfig.XL.Disk
	}
	// Validate collected uuids and verify JBOD.
	for _, uuid := range disks {
		if uuid == "" {
			continue
		}
		// Is uuid present on all JBOD ?.
		if !isSavedUUIDInOrder(uuid, formatConfigs) {
			return fmt.Errorf("%s disk not found in JBOD", uuid)
		}
	}
	return nil
}

// checkJBODConsistency - validate xl jbod order if they are consistent.
func checkJBODConsistency(formatConfigs []*formatConfigV1) error {
	var jbodStr string
	// Extract first valid JBOD.
	for _, format := range formatConfigs {
		if format == nil {
			continue
		}
		jbodStr = strings.Join(format.XL.JBOD, ".")
		break
	}
	for _, format := range formatConfigs {
		if format == nil {
			continue
		}
		savedJBODStr := strings.Join(format.XL.JBOD, ".")
		if jbodStr != savedJBODStr {
			return errors.New("Inconsistent JBOD found.")
		}
	}
	return nil
}

// findDiskIndex returns position of disk in JBOD.
func findDiskIndex(disk string, jbod []string) int {
	for index, uuid := range jbod {
		if uuid == disk {
			return index
		}
	}
	return -1
}

// reorderDisks - reorder disks in JBOD order.
func reorderDisks(bootstrapDisks []StorageAPI, formatConfigs []*formatConfigV1) ([]StorageAPI, error) {
	var savedJBOD []string
	for _, format := range formatConfigs {
		if format == nil {
			continue
		}
		savedJBOD = format.XL.JBOD
		break
	}
	// Pick the first JBOD list to verify the order and construct new set of disk slice.
	var newDisks = make([]StorageAPI, len(bootstrapDisks))
	for fIndex, format := range formatConfigs {
		if format == nil {
			continue
		}
		jIndex := findDiskIndex(format.XL.Disk, savedJBOD)
		if jIndex == -1 {
			return nil, errors.New("Unrecognized uuid " + format.XL.Disk + " found")
		}
		newDisks[jIndex] = bootstrapDisks[fIndex]
	}
	return newDisks, nil
}

// loadFormat - loads format.json from disk.
func loadFormat(disk StorageAPI) (format *formatConfigV1, err error) {
	var buffer []byte
	buffer, err = readAll(disk, minioMetaBucket, formatConfigFile)
	if err != nil {
		// 'file not found' and 'volume not found' as
		// same. 'volume not found' usually means its a fresh disk.
		if err == errFileNotFound || err == errVolumeNotFound {
			var vols []VolInfo
			vols, err = disk.ListVols()
			if err != nil {
				return nil, err
			}
			if len(vols) > 1 {
				// 'format.json' not found, but we found user data.
				return nil, errCorruptedFormat
			}
			// No other data found, its a fresh disk.
			return nil, errUnformattedDisk
		}
		return nil, err
	}
	format = &formatConfigV1{}
	err = json.Unmarshal(buffer, format)
	if err != nil {
		return nil, err
	}
	return format, nil
}

// isFormatNotFound - returns true if all `format.json` are not
// found on all disks.
func isFormatNotFound(formats []*formatConfigV1) bool {
	for _, format := range formats {
		// One of the `format.json` is found.
		if format != nil {
			return false
		}
	}
	// All format.json missing, success.
	return true
}

// isFormatFound - returns true if all input formats are found on
// all disks.
func isFormatFound(formats []*formatConfigV1) bool {
	for _, format := range formats {
		// One of `format.json` is not found.
		if format == nil {
			return false
		}
	}
	// All format.json present, success.
	return true
}

// Heals any missing format.json on the drives. Returns error only for unexpected errors
// as regular errors can be ignored since there might be enough quorum to be operational.
func healFormatXL(storageDisks []StorageAPI) error {
	formatConfigs := make([]*formatConfigV1, len(storageDisks))
	var referenceConfig *formatConfigV1
	// Loads `format.json` from all disks.
	for index, disk := range storageDisks {
		formatXL, err := loadFormat(disk)
		if err != nil {
			if err == errUnformattedDisk {
				// format.json is missing, should be healed.
				continue
			} else if err == errDiskNotFound { // Is a valid case we
				// can proceed without healing.
				return nil
			}
			// Return error for unsupported errors.
			return err
		} // Success.
		formatConfigs[index] = formatXL
	}
	// All `format.json` has been read successfully, previously completed.
	if isFormatFound(formatConfigs) {
		// Return success.
		return nil
	}
	// All disks are fresh, format.json will be written by initFormatXL()
	if isFormatNotFound(formatConfigs) {
		return initFormatXL(storageDisks)
	}
	// Validate format configs for consistency in JBOD and disks.
	if err := checkFormatXL(formatConfigs); err != nil {
		return err
	}

	if referenceConfig == nil {
		// This config will be used to update the drives missing format.json.
		for _, formatConfig := range formatConfigs {
			if formatConfig == nil {
				continue
			}
			referenceConfig = formatConfig
			break
		}
	}

	// Collect new format configs.
	var newFormatConfigs = make([]*formatConfigV1, len(storageDisks))

	// Collect new JBOD.
	newJBOD := referenceConfig.XL.JBOD

	// This section heals the format.json and updates the fresh disks
	// by apply a new UUID for all the fresh disks.
	for index, format := range formatConfigs {
		if format == nil {
			newJBOD[index] = getUUID()
		}
	}
	// Collect new format configs that need to be written.
	for index, format := range formatConfigs {
		if format == nil {
			config := &formatConfigV1{
				Version: referenceConfig.Version,
				Format:  referenceConfig.Format,
				XL: &xlFormat{
					Version: referenceConfig.XL.Version,
					Disk:    newJBOD[index],
					JBOD:    newJBOD,
				},
			}
			newFormatConfigs[index] = config
			continue
		}
		newFormatConfigs[index] = format
		newFormatConfigs[index].XL.JBOD = newJBOD
		newFormatConfigs[index].XL.Disk = newJBOD[index]
	}
	// Save new `format.json` across all disks.
	return saveFormatXL(storageDisks, newFormatConfigs)
}

// loadFormatXL - loads XL `format.json` and returns back properly
// ordered storage slice based on `format.json`.
func loadFormatXL(bootstrapDisks []StorageAPI) (disks []StorageAPI, err error) {
	var unformattedDisksFoundCnt = 0
	var diskNotFoundCount = 0
	formatConfigs := make([]*formatConfigV1, len(bootstrapDisks))

	// Try to load `format.json` bootstrap disks.
	for index, disk := range bootstrapDisks {
		var formatXL *formatConfigV1
		formatXL, err = loadFormat(disk)
		if err != nil {
			if err == errUnformattedDisk {
				unformattedDisksFoundCnt++
				continue
			} else if err == errDiskNotFound {
				diskNotFoundCount++
				continue
			}
			return nil, err
		}
		// Save valid formats.
		formatConfigs[index] = formatXL
	}

	// If all disks indicate that 'format.json' is not available
	// return 'errUnformattedDisk'.
	if unformattedDisksFoundCnt == len(bootstrapDisks) {
		return nil, errUnformattedDisk
	} else if diskNotFoundCount == len(bootstrapDisks) {
		return nil, errDiskNotFound
	} else if diskNotFoundCount > len(bootstrapDisks)-(len(bootstrapDisks)/2+1) {
		return nil, errXLReadQuorum
	} else if unformattedDisksFoundCnt > len(bootstrapDisks)-(len(bootstrapDisks)/2+1) {
		return nil, errXLReadQuorum
	}

	// Validate the format configs read are correct.
	if err = checkFormatXL(formatConfigs); err != nil {
		return nil, err
	}
	// Erasure code requires disks to be presented in the same order each time.
	return reorderDisks(bootstrapDisks, formatConfigs)
}

// checkFormatXL - verifies if format.json format is intact.
func checkFormatXL(formatConfigs []*formatConfigV1) error {
	for _, formatXL := range formatConfigs {
		if formatXL == nil {
			continue
		}
		// Validate format version and format type.
		if formatXL.Version != "1" {
			return fmt.Errorf("Unsupported version of backend format [%s] found.", formatXL.Version)
		}
		if formatXL.Format != "xl" {
			return fmt.Errorf("Unsupported backend format [%s] found.", formatXL.Format)
		}
		if formatXL.XL.Version != "1" {
			return fmt.Errorf("Unsupported XL backend format found [%s]", formatXL.XL.Version)
		}
		if len(formatConfigs) != len(formatXL.XL.JBOD) {
			return fmt.Errorf("Number of disks %d did not match the backend format %d", len(formatConfigs), len(formatXL.XL.JBOD))
		}
	}
	if err := checkJBODConsistency(formatConfigs); err != nil {
		return err
	}
	return checkDisksConsistency(formatConfigs)
}

// saveFormatXL - populates `format.json` on disks in its order.
func saveFormatXL(storageDisks []StorageAPI, formats []*formatConfigV1) error {
	var errs = make([]error, len(storageDisks))
	var wg = &sync.WaitGroup{}
	// Write `format.json` to all disks.
	for index, disk := range storageDisks {
		if disk == nil {
			continue
		}
		wg.Add(1)
		go func(index int, disk StorageAPI, format *formatConfigV1) {
			defer wg.Done()

			// Marshal and write to disk.
			formatBytes, err := json.Marshal(format)
			if err != nil {
				errs[index] = err
				return
			}

			// Purge any existing temporary file, okay to ignore errors here.
			disk.DeleteFile(minioMetaBucket, formatConfigFileTmp)

			// Append file `format.json.tmp`.
			if err = disk.AppendFile(minioMetaBucket, formatConfigFileTmp, formatBytes); err != nil {
				errs[index] = err
				return
			}
			// Rename file `format.json.tmp` --> `format.json`.
			if err = disk.RenameFile(minioMetaBucket, formatConfigFileTmp, minioMetaBucket, formatConfigFile); err != nil {
				errs[index] = err
				return
			}
		}(index, disk, formats[index])
	}

	// Wait for the routines to finish.
	wg.Wait()

	// Validate if we encountered any errors, return quickly.
	for _, err := range errs {
		if err != nil {
			// Failure.
			return err
		}
	}

	// Success.
	return nil
}

// initFormatXL - save XL format configuration on all disks.
func initFormatXL(storageDisks []StorageAPI) (err error) {
	// Initialize jbods.
	var jbod = make([]string, len(storageDisks))

	// Initialize formats.
	var formats = make([]*formatConfigV1, len(storageDisks))

	// Initialize `format.json`.
	for index, disk := range storageDisks {
		if disk == nil {
			continue
		}
		// Allocate format config.
		formats[index] = &formatConfigV1{
			Version: "1",
			Format:  "xl",
			XL: &xlFormat{
				Version: "1",
				Disk:    getUUID(),
			},
		}
		jbod[index] = formats[index].XL.Disk
	}

	// Update the jbod entries.
	for index, disk := range storageDisks {
		if disk == nil {
			continue
		}
		// Save jbod.
		formats[index].XL.JBOD = jbod
	}

	// Save formats `format.json` across all disks.
	return saveFormatXL(storageDisks, formats)
}
