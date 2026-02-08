package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30

	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video Not Found", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized user", nil)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video from request", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	mediaType, _, err = mime.ParseMediaType(mediaType)

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Media type not compatible", nil)
		return
	}
	ext := strings.Split(mediaType, "/")

	randomFileName := make([]byte, 32)
	_, err = rand.Read(randomFileName)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating thumbnail filename", err)
		return
	}
	filename := base64.RawURLEncoding.EncodeToString(randomFileName) + "." + ext[1]

	tempFile, err := os.CreateTemp("", filename)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Issue creating temporary file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err = io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusBadRequest, "Issue copying to temporary file", err)
		return
	}

	tempFileProcessed, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Issue processing file for fast start", err)
		return
	}
	processedFilenameSlice := strings.Split(tempFileProcessed, "/")
	processedFilename := processedFilenameSlice[(len(processedFilenameSlice) - 1)]

	processedFile, err := os.Open(tempFileProcessed)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Could not find processed file", err)
		return
	}
	defer processedFile.Close()

	aspectRatio, err := getAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Issue finding aspect ratio", err)
		return
	}
	fileKey := aspectRatio + "/" + processedFilename

	tempFile.Seek(0, io.SeekStart)
	cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        processedFile,
		ContentType: &mediaType,
	})
	url := fmt.Sprintf("%s,%s", cfg.s3Bucket, fileKey)

	video.VideoURL = &url
	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to create signed URL", err)
		return
	}

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Video unable to be updated", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getAspectRatio(filePath string) (string, error) {
	type respStruct struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	var stdoutBuffer bytes.Buffer

	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	cmd.Stdout = &stdoutBuffer

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	data := &respStruct{}

	err = json.Unmarshal(stdoutBuffer.Bytes(), data)
	if err != nil {
		return "", err
	}
	if len(data.Streams) == 0 || data.Streams[0].Height == 0 {
		return "", fmt.Errorf("No valid streams in ffprobe output")
	}
	aspRatio := float64(data.Streams[0].Width) / float64(data.Streams[0].Height)
	aspRatioStr := getAspectRatioString(aspRatio)
	return aspRatioStr, nil
}

func getAspectRatioString(aspectRatio float64) string {
	if math.Round(aspectRatio*100)/100 == math.Round(1600.0/9.0)/100.0 {
		return "landscape"
	} else if math.Round(aspectRatio*100)/100 == math.Round(900.0/16.0)/100.0 {
		return "portrait"
	}

	return "other"
}

func processVideoForFastStart(filepath string) (string, error) {
	outputFilepath := filepath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilepath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return outputFilepath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	params := s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}
	presignedClient := s3.NewPresignClient(s3Client)
	req, err := presignedClient.PresignGetObject(context.TODO(), &params, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}

	return req.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	videoURLSlice := strings.Split(*video.VideoURL, ",")
	if len(videoURLSlice) != 2 {
		return video, nil
	}
	bucket := videoURLSlice[0]
	key := videoURLSlice[1]
	signedVideoURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 10*time.Minute)
	if err != nil {
		return video, err
	}

	video.VideoURL = &signedVideoURL

	return video, nil
}
