package main

import (
	"mime"
	"net/http"
	"os"
	"os/exec"
	"io"
	"fmt"
	"errors"
	"encoding/json"
	"bytes"
	"time"
	"context"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxSize = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxSize)

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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't retrieve video data", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You do not own this video", err)
		return
	}

	fmt.Println("uploading mp4 for video", videoID, "by user", userID)

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't parse video file", err)
		return
	}
	defer file.Close()

	fileType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse content type", err)
		return
	}
	if fileType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	key, err := auth.MakeByteHex(32)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create video key", err)
		return
	}

	io.Copy(tmpFile, file)
	tmpFile.Seek(0, io.SeekStart)
	detectedAspectRatio, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}

	var aspectRatio string
	switch detectedAspectRatio {
		case "16:9":
			aspectRatio = "landscape"
		case "9:16":
			aspectRatio = "portrait"
		default:
			aspectRatio = "other"
	}

	processedFilePath, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video", err)
		return
	}

	videoKey := aspectRatio + "/" + key + ".mp4"
	params := s3.PutObjectInput{
		Bucket: &cfg.s3Bucket,
		Key: &videoKey,
		Body: processedFile,
		ContentType: &fileType,
	}

	_, err = cfg.s3Client.PutObject(r.Context(), &params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create S3 object", err)
		return
	}

	newVidURL := cfg.s3Bucket + "," + videoKey
	video.VideoURL = &newVidURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video database entry", err)
		return
	}
	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to generate signed URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signedVideo)
}


func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	splitURL := strings.Split(*video.VideoURL, ",")
	bucket := splitURL[0]
	key := splitURL[1]

	signedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 5 * time.Minute)
	if err != nil {
		return video, errors.New("failed to generate signed URL: " + err.Error())
	}

	video.VideoURL = &signedURL
	return video, nil
}


func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v",
		"error",
		"-select_streams",
		"v:0",
		"-print_format",
		"json",
		"-show_streams",
		filepath,
	)

	buffer := &bytes.Buffer{}
	cmd.Stdout = buffer
	if err := cmd.Run(); err != nil {
		return "", err
	}

	type ratio struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	decoder := json.NewDecoder(buffer)
	params := &ratio{}
	if err := decoder.Decode(params); err != nil {
		return "", err
	}
	if len(params.Streams) == 0 {
		return "", errors.New("no streams in video file")
	}

	aspectRatio := float64(params.Streams[0].Width) / float64(params.Streams[0].Height)
	if aspectRatio > 1.7 && aspectRatio < 1.8 {
		return "16:9", nil
	} else if aspectRatio > 0.5 && aspectRatio < 0.6 {
		return "9:16", nil
	}
	return "other", nil
}


func processVideoForFastStart(filepath string) (string, error) {
	outputPath := filepath + ".processing"
	cmd := exec.Command(
		"ffmpeg",
		"-i",
		filepath,
		"-c",
		"copy",
		"-movflags",
		"faststart",
		"-f",
		"mp4",
		outputPath,
	)

	if err := cmd.Run(); err != nil {
		return "", err
	}
	return outputPath, nil
}


func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	client := s3.NewPresignClient(s3Client)
	req, err := client.PresignGetObject(
		context.Background(),
		&s3.GetObjectInput{
			Bucket: &bucket,
			Key:    &key,
		},
		s3.WithPresignExpires(expireTime),
	)

	if err != nil {
		return "", err
	}
	return req.URL, nil
}
