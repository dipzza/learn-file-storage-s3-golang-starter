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
		respondWithError(w, http.StatusNotFound, "Couldn't get video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "User not authorized to edit video", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	r.Body = http.MaxBytesReader(w, r.Body, 1 << 30)
	formFile, formFileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse multipart form", err)
		return
	}
	defer formFile.Close()

	contentType := formFileHeader.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Unsupported file type", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, formFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file", err)
		return
	}

	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't seek file", err)
		return
	}

	fastStartTempFilepath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}
	fastTempFile, err := os.Open(fastStartTempFilepath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open fast start temp file", err)
		return
	}
	defer os.Remove(fastStartTempFilepath)
	defer fastTempFile.Close()

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	randomBytes := make([]byte, 32)
	rand.Read(randomBytes)
	randomString := base64.RawURLEncoding.EncodeToString(randomBytes)
	extension := getExtension(mediaType)
	fileKey := fmt.Sprintf("%s/%s.%s", aspectRatio, randomString, extension)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket: &cfg.s3Bucket,
		Key:    &fileKey,
		Body:   fastTempFile,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload file", err)
		return
	}

	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, fileKey)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't sign video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signedVideo)
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	
	params := strings.Split(*video.VideoURL, ",")
	bucket := params[0]
	key := params[1]

	url, err := generatePresignedURL(cfg.s3Client, bucket, key, 10*time.Minute)
	if err != nil {
		return database.Video{}, err
	}

	video.VideoURL = &url
	return video, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	stdoutBuffer := bytes.Buffer{}
	cmd.Stdout = &stdoutBuffer
	if err := cmd.Run(); err != nil {
		return "", err
	}

	type ffprobeStream struct {
		Streams []struct {
			Width              int    `json:"width"`
			Height             int    `json:"height"`
		} `json:"streams"`
	}
	var cmdResult ffprobeStream
	if err := json.Unmarshal(stdoutBuffer.Bytes(), &cmdResult); err != nil {
		return "", err
	}
	width := cmdResult.Streams[0].Width
	height := cmdResult.Streams[0].Height
	ratio := float64(width) / float64(height)

	if (math.Abs(ratio -  16.0/9.0) < 0.001) {
		return "landscape", nil
	} else if (math.Abs(ratio -  9.0/16.0) < 0.001) {
		return "portrait", nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filepath string) (string, error) {
	output_path := filepath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", output_path)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return output_path, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket string, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	presignReq, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: &bucket,
		Key: &key,
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}
	return presignReq.URL, nil
}