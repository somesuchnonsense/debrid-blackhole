package realdebrid

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"io"
	"net/http"
	gourl "net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type RealDebrid struct {
	Name string
	Host string `json:"host"`

	APIKey             string
	currentDownloadKey string
	accounts           map[string]types.Account
	accountsMutex      sync.RWMutex

	DownloadUncached bool
	client           *request.Client
	downloadClient   *request.Client

	MountPath   string
	logger      zerolog.Logger
	checkCached bool
	addSamples  bool
}

func New(dc config.Debrid) *RealDebrid {
	rl := request.ParseRateLimit(dc.RateLimit)

	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", dc.APIKey),
	}
	_log := logger.New(dc.Name)

	accounts := make(map[string]types.Account)
	currentDownloadKey := dc.DownloadAPIKeys[0]
	for idx, key := range dc.DownloadAPIKeys {
		id := strconv.Itoa(idx)
		accounts[id] = types.Account{
			Name:  key,
			ID:    id,
			Token: key,
		}
	}

	downloadHeaders := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", currentDownloadKey),
	}

	return &RealDebrid{
		Name:             "realdebrid",
		Host:             "https://api.real-debrid.com/rest/1.0",
		APIKey:           dc.APIKey,
		accounts:         accounts,
		DownloadUncached: dc.DownloadUncached,
		client: request.New(
			request.WithHeaders(headers),
			request.WithRateLimiter(rl),
			request.WithLogger(_log),
			request.WithMaxRetries(5),
			request.WithRetryableStatus(429, 502),
			request.WithProxy(dc.Proxy),
		),
		downloadClient: request.New(
			request.WithHeaders(downloadHeaders),
			request.WithLogger(_log),
			request.WithMaxRetries(10),
			request.WithRetryableStatus(429, 447, 502),
			request.WithProxy(dc.Proxy),
		),
		currentDownloadKey: currentDownloadKey,
		MountPath:          dc.Folder,
		logger:             logger.New(dc.Name),
		checkCached:        dc.CheckCached,
		addSamples:         dc.AddSamples,
	}
}

func (r *RealDebrid) GetName() string {
	return r.Name
}

func (r *RealDebrid) GetLogger() zerolog.Logger {
	return r.logger
}

func getSelectedFiles(t *types.Torrent, data torrentInfo) map[string]types.File {
	selectedFiles := make([]types.File, 0)
	for _, f := range data.Files {
		if f.Selected == 1 {
			name := filepath.Base(f.Path)
			file := types.File{
				TorrentId: t.Id,
				Name:      name,
				Path:      name,
				Size:      f.Bytes,
				Id:        strconv.Itoa(f.ID),
			}
			selectedFiles = append(selectedFiles, file)
		}
	}
	files := make(map[string]types.File)
	for index, f := range selectedFiles {
		if index >= len(data.Links) {
			break
		}
		f.Link = data.Links[index]
		files[f.Name] = f
	}
	return files
}

// getTorrentFiles returns a list of torrent files from the torrent info
// validate is used to determine if the files should be validated
// if validate is false, selected files will be returned
func (r *RealDebrid) getTorrentFiles(t *types.Torrent, data torrentInfo) map[string]types.File {
	files := make(map[string]types.File)
	cfg := config.Get()
	idx := 0

	for _, f := range data.Files {
		name := filepath.Base(f.Path)
		if !r.addSamples && utils.IsSampleFile(f.Path) {
			// Skip sample files
			continue
		}

		if !cfg.IsAllowedFile(name) {
			continue
		}
		if !cfg.IsSizeAllowed(f.Bytes) {
			continue
		}

		file := types.File{
			TorrentId: t.Id,
			Name:      name,
			Path:      name,
			Size:      f.Bytes,
			Id:        strconv.Itoa(f.ID),
		}
		files[name] = file
		idx++
	}
	return files
}

func (r *RealDebrid) IsAvailable(hashes []string) map[string]bool {
	// Check if the infohashes are available in the local cache
	result := make(map[string]bool)

	// Divide hashes into groups of 100
	for i := 0; i < len(hashes); i += 200 {
		end := i + 200
		if end > len(hashes) {
			end = len(hashes)
		}

		// Filter out empty strings
		validHashes := make([]string, 0, end-i)
		for _, hash := range hashes[i:end] {
			if hash != "" {
				validHashes = append(validHashes, hash)
			}
		}

		// If no valid hashes in this batch, continue to the next batch
		if len(validHashes) == 0 {
			continue
		}

		hashStr := strings.Join(validHashes, "/")
		url := fmt.Sprintf("%s/torrents/instantAvailability/%s", r.Host, hashStr)
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := r.client.MakeRequest(req)
		if err != nil {
			r.logger.Info().Msgf("Error checking availability: %v", err)
			return result
		}
		var data AvailabilityResponse
		err = json.Unmarshal(resp, &data)
		if err != nil {
			r.logger.Info().Msgf("Error marshalling availability: %v", err)
			return result
		}
		for _, h := range hashes[i:end] {
			hosters, exists := data[strings.ToLower(h)]
			if exists && len(hosters.Rd) > 0 {
				result[h] = true
			}
		}
	}
	return result
}

func (r *RealDebrid) SubmitMagnet(t *types.Torrent) (*types.Torrent, error) {
	if t.Magnet.IsTorrent() {
		return r.addTorrent(t)
	}
	return r.addMagnet(t)
}

func (r *RealDebrid) addTorrent(t *types.Torrent) (*types.Torrent, error) {
	url := fmt.Sprintf("%s/torrents/addTorrent", r.Host)
	var data AddMagnetSchema
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(t.Magnet.File))

	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/x-bittorrent")
	resp, err := r.client.MakeRequest(req)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(resp, &data); err != nil {
		return nil, err
	}
	t.Id = data.Id
	t.Debrid = r.Name
	t.MountPath = r.MountPath
	return t, nil
}

func (r *RealDebrid) addMagnet(t *types.Torrent) (*types.Torrent, error) {
	url := fmt.Sprintf("%s/torrents/addMagnet", r.Host)
	payload := gourl.Values{
		"magnet": {t.Magnet.Link},
	}
	var data AddMagnetSchema
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(payload.Encode()))
	resp, err := r.client.MakeRequest(req)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(resp, &data); err != nil {
		return nil, err
	}
	t.Id = data.Id
	t.Debrid = r.Name
	t.MountPath = r.MountPath
	return t, nil
}

func (r *RealDebrid) GetTorrent(torrentId string) (*types.Torrent, error) {
	url := fmt.Sprintf("%s/torrents/info/%s", r.Host, torrentId)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, request.TorrentNotFoundError
		}
		return nil, fmt.Errorf("realdebrid API error: Status: %d || Body: %s", resp.StatusCode, string(bodyBytes))
	}
	var data torrentInfo
	err = json.Unmarshal(bodyBytes, &data)
	if err != nil {
		return nil, err
	}
	t := &types.Torrent{
		Id:               data.ID,
		Name:             data.Filename,
		Bytes:            data.Bytes,
		Folder:           data.OriginalFilename,
		Progress:         data.Progress,
		Speed:            data.Speed,
		Seeders:          data.Seeders,
		Added:            data.Added,
		Status:           data.Status,
		Filename:         data.Filename,
		OriginalFilename: data.OriginalFilename,
		Links:            data.Links,
		Debrid:           r.Name,
		MountPath:        r.MountPath,
	}
	t.Files = r.getTorrentFiles(t, data) // Get selected files
	return t, nil
}

func (r *RealDebrid) UpdateTorrent(t *types.Torrent) error {
	url := fmt.Sprintf("%s/torrents/info/%s", r.Host, t.Id)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return request.TorrentNotFoundError
		}
		return fmt.Errorf("realdebrid API error: Status: %d || Body: %s", resp.StatusCode, string(bodyBytes))
	}
	var data torrentInfo
	err = json.Unmarshal(bodyBytes, &data)
	if err != nil {
		return err
	}
	t.Name = data.Filename
	t.Bytes = data.Bytes
	t.Folder = data.OriginalFilename
	t.Progress = data.Progress
	t.Status = data.Status
	t.Speed = data.Speed
	t.Seeders = data.Seeders
	t.Filename = data.Filename
	t.OriginalFilename = data.OriginalFilename
	t.Links = data.Links
	t.MountPath = r.MountPath
	t.Debrid = r.Name
	t.Added = data.Added
	t.Files = getSelectedFiles(t, data) // Get selected files
	return nil
}

func (r *RealDebrid) CheckStatus(t *types.Torrent, isSymlink bool) (*types.Torrent, error) {
	url := fmt.Sprintf("%s/torrents/info/%s", r.Host, t.Id)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for {
		resp, err := r.client.MakeRequest(req)
		if err != nil {
			r.logger.Info().Msgf("ERROR Checking file: %v", err)
			return t, err
		}
		var data torrentInfo
		if err = json.Unmarshal(resp, &data); err != nil {
			return t, err
		}
		status := data.Status
		t.Name = data.Filename // Important because some magnet changes the name
		t.Folder = data.OriginalFilename
		t.Filename = data.Filename
		t.OriginalFilename = data.OriginalFilename
		t.Bytes = data.Bytes
		t.Progress = data.Progress
		t.Speed = data.Speed
		t.Seeders = data.Seeders
		t.Links = data.Links
		t.Status = status
		t.Debrid = r.Name
		t.MountPath = r.MountPath
		if status == "waiting_files_selection" {
			t.Files = r.getTorrentFiles(t, data)
			if len(t.Files) == 0 {
				return t, fmt.Errorf("no video files found")
			}
			filesId := make([]string, 0)
			for _, f := range t.Files {
				filesId = append(filesId, f.Id)
			}
			p := gourl.Values{
				"files": {strings.Join(filesId, ",")},
			}
			payload := strings.NewReader(p.Encode())
			req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/torrents/selectFiles/%s", r.Host, t.Id), payload)
			res, err := r.client.Do(req)
			if err != nil {
				return t, err
			}
			if res.StatusCode != http.StatusNoContent {
				return t, fmt.Errorf("realdebrid API error: Status: %d", res.StatusCode)
			}
		} else if status == "downloaded" {
			t.Files = getSelectedFiles(t, data) // Get selected files
			r.logger.Info().Msgf("Torrent: %s downloaded to RD", t.Name)
			if !isSymlink {
				err = r.GenerateDownloadLinks(t)
				if err != nil {
					return t, err
				}
			}
			break
		} else if utils.Contains(r.GetDownloadingStatus(), status) {
			if !t.DownloadUncached {
				return t, fmt.Errorf("torrent: %s not cached", t.Name)
			}
			return t, nil
		} else {
			return t, fmt.Errorf("torrent: %s has error: %s", t.Name, status)
		}

	}
	return t, nil
}

func (r *RealDebrid) DeleteTorrent(torrentId string) error {
	url := fmt.Sprintf("%s/torrents/delete/%s", r.Host, torrentId)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if _, err := r.client.MakeRequest(req); err != nil {
		return err
	}
	r.logger.Info().Msgf("Torrent: %s deleted from RD", torrentId)
	return nil
}

func (r *RealDebrid) GenerateDownloadLinks(t *types.Torrent) error {
	filesCh := make(chan types.File, len(t.Files))
	errCh := make(chan error, len(t.Files))

	var wg sync.WaitGroup
	wg.Add(len(t.Files))
	for _, f := range t.Files {
		go func(file types.File) {
			defer wg.Done()

			link, err := r.GetDownloadLink(t, &file)
			if err != nil {
				errCh <- err
				return
			}

			file.DownloadLink = link
			filesCh <- file
		}(f)
	}

	go func() {
		wg.Wait()
		close(filesCh)
		close(errCh)
	}()

	// Collect results
	files := make(map[string]types.File, len(t.Files))
	for file := range filesCh {
		files[file.Name] = file
	}

	// Check for errors
	for err := range errCh {
		if err != nil {
			return err // Return the first error encountered
		}
	}

	t.Files = files
	return nil
}

func (r *RealDebrid) CheckLink(link string) error {
	url := fmt.Sprintf("%s/unrestrict/check", r.Host)
	payload := gourl.Values{
		"link": {link},
	}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(payload.Encode()))
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return request.HosterUnavailableError // File has been removed
	}
	return nil
}

func (r *RealDebrid) _getDownloadLink(file *types.File) (*types.DownloadLink, error) {
	url := fmt.Sprintf("%s/unrestrict/link/", r.Host)
	payload := gourl.Values{
		"link": {file.Link},
	}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(payload.Encode()))
	resp, err := r.downloadClient.Do(req)

	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Read the response body to get the error message
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var data ErrorResponse
		if err = json.Unmarshal(b, &data); err != nil {
			return nil, fmt.Errorf("error unmarshalling %d || %s \n %s", resp.StatusCode, err, string(b))
		}
		switch data.ErrorCode {
		case 19:
			return nil, request.HosterUnavailableError // File has been removed
		case 23:
			return nil, request.TrafficExceededError
		case 24:
			return nil, request.HosterUnavailableError // Link has been nerfed
		case 34:
			return nil, request.TrafficExceededError // traffic exceeded
		case 35:
			return nil, request.HosterUnavailableError
		case 36:
			return nil, request.TrafficExceededError // traffic exceeded
		default:
			return nil, fmt.Errorf("realdebrid API error: Status: %d || Code: %d", resp.StatusCode, data.ErrorCode)
		}
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var data UnrestrictResponse
	if err = json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("realdebrid API error: Error unmarshalling response: %w", err)
	}
	if data.Download == "" {
		return nil, fmt.Errorf("realdebrid API error: download link not found")
	}
	return &types.DownloadLink{
		Filename:     data.Filename,
		Size:         data.Filesize,
		Link:         data.Link,
		DownloadLink: data.Download,
		Generated:    time.Now(),
	}, nil

}

func (r *RealDebrid) GetDownloadLink(t *types.Torrent, file *types.File) (*types.DownloadLink, error) {

	if r.currentDownloadKey == "" {
		// If no download key is set, use the first one
		accounts := r.getActiveAccounts()
		if len(accounts) < 1 {
			// No active download keys. It's likely that the key has reached bandwidth limit
			return nil, fmt.Errorf("no active download keys")
		}
		r.currentDownloadKey = accounts[0].Token
	}

	r.downloadClient.SetHeader("Authorization", fmt.Sprintf("Bearer %s", r.currentDownloadKey))
	downloadLink, err := r._getDownloadLink(file)
	retries := 0
	if err != nil {
		if errors.Is(err, request.TrafficExceededError) {
			// Retries generating
			retries = 5
		} else {
			// If the error is not traffic exceeded, return the error
			return nil, err
		}
	}
	backOff := 1 * time.Second
	for retries > 0 {
		downloadLink, err = r._getDownloadLink(file)
		if err == nil {
			return downloadLink, nil
		}
		if !errors.Is(err, request.TrafficExceededError) {
			return nil, err
		}
		// Add a delay before retrying
		time.Sleep(backOff)
		backOff *= 2 // Exponential backoff
	}
	return downloadLink, nil
}

func (r *RealDebrid) GetCheckCached() bool {
	return r.checkCached
}

func (r *RealDebrid) getTorrents(offset int, limit int) (int, []*types.Torrent, error) {
	url := fmt.Sprintf("%s/torrents?limit=%d", r.Host, limit)
	torrents := make([]*types.Torrent, 0)
	if offset > 0 {
		url = fmt.Sprintf("%s&offset=%d", url, offset)
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := r.client.Do(req)

	if err != nil {
		return 0, torrents, err
	}

	if resp.StatusCode == http.StatusNoContent {
		return 0, torrents, nil
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return 0, torrents, fmt.Errorf("realdebrid API error: %d", resp.StatusCode)
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, torrents, err
	}
	totalItems, _ := strconv.Atoi(resp.Header.Get("X-Total-Count"))
	var data []TorrentsResponse
	if err = json.Unmarshal(body, &data); err != nil {
		return 0, torrents, err
	}
	filenames := map[string]struct{}{}
	for _, t := range data {
		if t.Status != "downloaded" {
			continue
		}
		torrents = append(torrents, &types.Torrent{
			Id:               t.Id,
			Name:             t.Filename,
			Bytes:            t.Bytes,
			Progress:         t.Progress,
			Status:           t.Status,
			Filename:         t.Filename,
			OriginalFilename: t.Filename,
			Links:            t.Links,
			Files:            make(map[string]types.File),
			InfoHash:         t.Hash,
			Debrid:           r.Name,
			MountPath:        r.MountPath,
			Added:            t.Added.Format(time.RFC3339),
		})
		filenames[t.Filename] = struct{}{}
	}
	return totalItems, torrents, nil
}

func (r *RealDebrid) GetTorrents() ([]*types.Torrent, error) {
	limit := 5000

	// Get first batch and total count
	allTorrents := make([]*types.Torrent, 0)
	var fetchError error
	offset := 0
	for {
		// Fetch next batch of torrents
		_, torrents, err := r.getTorrents(offset, limit)
		if err != nil {
			fetchError = err
			break
		}
		totalTorrents := len(torrents)
		if totalTorrents == 0 {
			break
		}
		allTorrents = append(allTorrents, torrents...)
		offset += totalTorrents
	}

	if fetchError != nil {
		return nil, fetchError
	}

	return allTorrents, nil
}

func (r *RealDebrid) GetDownloads() (map[string]types.DownloadLink, error) {
	links := make(map[string]types.DownloadLink)
	offset := 0
	limit := 1000

	accounts := r.getActiveAccounts()

	if len(accounts) < 1 {
		// No active download keys. It's likely that the key has reached bandwidth limit
		return nil, fmt.Errorf("no active download keys")
	}
	r.downloadClient.SetHeader("Authorization", fmt.Sprintf("Bearer %s", accounts[0].Token))
	for {
		dl, err := r._getDownloads(offset, limit)
		if err != nil {
			break
		}
		if len(dl) == 0 {
			break
		}

		for _, d := range dl {
			if _, exists := links[d.Link]; exists {
				// This is ordered by date, so we can skip the rest
				continue
			}
			links[d.Link] = d
		}

		offset += len(dl)
	}
	return links, nil
}

func (r *RealDebrid) _getDownloads(offset int, limit int) ([]types.DownloadLink, error) {
	url := fmt.Sprintf("%s/downloads?limit=%d", r.Host, limit)
	if offset > 0 {
		url = fmt.Sprintf("%s&offset=%d", url, offset)
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := r.downloadClient.MakeRequest(req)
	if err != nil {
		return nil, err
	}
	var data []DownloadsResponse
	if err = json.Unmarshal(resp, &data); err != nil {
		return nil, err
	}
	links := make([]types.DownloadLink, 0)
	for _, d := range data {
		links = append(links, types.DownloadLink{
			Filename:     d.Filename,
			Size:         d.Filesize,
			Link:         d.Link,
			DownloadLink: d.Download,
			Generated:    d.Generated,
			Id:           d.Id,
		})

	}
	return links, nil
}

func (r *RealDebrid) GetDownloadingStatus() []string {
	return []string{"downloading", "magnet_conversion", "queued", "compressing", "uploading"}
}

func (r *RealDebrid) GetDownloadUncached() bool {
	return r.DownloadUncached
}

func (r *RealDebrid) GetMountPath() string {
	return r.MountPath
}

func (r *RealDebrid) DisableAccount(accountId string) {
	r.accountsMutex.Lock()
	defer r.accountsMutex.Unlock()
	if len(r.accounts) == 1 {
		r.logger.Info().Msgf("Cannot disable last account: %s", accountId)
		return
	}
	r.currentDownloadKey = ""
	if value, ok := r.accounts[accountId]; ok {
		value.Disabled = true
		r.accounts[accountId] = value
		r.logger.Info().Msgf("Disabled account Index: %s", value.ID)
	}
}

func (r *RealDebrid) ResetActiveDownloadKeys() {
	r.accountsMutex.Lock()
	defer r.accountsMutex.Unlock()
	for key, value := range r.accounts {
		value.Disabled = false
		r.accounts[key] = value
	}
}

func (r *RealDebrid) getActiveAccounts() []types.Account {
	r.accountsMutex.RLock()
	defer r.accountsMutex.RUnlock()
	accounts := make([]types.Account, 0)

	for _, value := range r.accounts {
		if value.Disabled {
			continue
		}
		accounts = append(accounts, value)
	}

	// Sort accounts by ID
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].ID < accounts[j].ID
	})
	return accounts
}

func (r *RealDebrid) DeleteDownloadLink(linkId string) error {
	url := fmt.Sprintf("%s/downloads/delete/%s", r.Host, linkId)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if _, err := r.downloadClient.MakeRequest(req); err != nil {
		return err
	}
	return nil
}
