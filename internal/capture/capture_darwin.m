// Screen capture and H.264 encoding on macOS.
//
// Captures a specific display with ScreenCaptureKit, encodes with VideoToolbox
// and hands Annex-B NAL units back to Go. Both are hardware accelerated, so
// the cost at 720x1280 is negligible.
//
// ScreenCaptureKit only delivers a frame when the display's contents change.
// For a USB panel that must be fed continuously that would stall on a static
// desktop, so this file re-emits the most recent frame on a timer when capture
// goes quiet.
//
// All state lives in a TURZXCaptureContext, of which a process may hold
// several: one per driven panel, each with its own display, encoder and frame
// queue. Nothing here is a singleton, so two captures cannot interfere.

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>
#include <mach/mach_time.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <VideoToolbox/VideoToolbox.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <Accelerate/Accelerate.h>
#import <dispatch/dispatch.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// Error codes surfaced to Go.
enum {
	TURZX_E_OK = 0,
	TURZX_E_NO_DISPLAY = 1,     // the display was not in ScreenCaptureKit's list
	TURZX_E_PERMISSION = 2,     // Screen Recording permission not granted
	TURZX_E_CAPTURE = 3,        // SCStream failed to start
	TURZX_E_ENCODER = 4,        // VideoToolbox session could not be created
	TURZX_E_TIMEOUT = 5,        // start did not complete in time
};

#define TURZX_QUEUE_SLOTS 6
#define TURZX_FRAME_MAX (512 * 1024)

// TURZXCaptureContext is one capture: a display, an encoder and the queue of
// encoded frames waiting to be read. The pointer Go holds is opaque.
typedef struct {
	pthread_mutex_t lock;

	// Encoded frame queue. Full means drop the oldest: a live display should
	// show the newest content, not fall behind.
	uint8_t *frames[TURZX_QUEUE_SLOTS];
	size_t frameLen[TURZX_QUEUE_SLOTS];
	int head, tail, count;

	int lastError; // 0 = ok, else a TURZX_E_* code
	bool running;

	// quarterTurns is how many quarter turns clockwise the captured frames
	// need before they match the panel's native orientation. Zero means the
	// panel is mounted the way it comes and no rotation is applied at all.
	int quarterTurns;

	// encodeBuf assembles one Annex-B frame. Shared by the encoder callback,
	// which is serialised by VideoToolbox against one session, and freed with
	// the context.
	uint8_t *encodeBuf;

	SCStream *stream;
	id output;                 // TURZXStreamOutput, held as id for ARC
	VTCompressionSessionRef encoder;
} TURZXCaptureContext;

// --- encoded frame queue ---------------------------------------------------

static void ctxPush(TURZXCaptureContext *ctx, const uint8_t *data, size_t len) {
	if (len == 0 || len > TURZX_FRAME_MAX) return;
	pthread_mutex_lock(&ctx->lock);
	int slot = ctx->head;
	if (ctx->count == TURZX_QUEUE_SLOTS) {
		// Full: drop the oldest frame. A dashboard-like display should show
		// the newest content, not fall behind.
		ctx->tail = (ctx->tail + 1) % TURZX_QUEUE_SLOTS;
		ctx->count--;
	}
	if (ctx->frames[slot] == NULL) {
		ctx->frames[slot] = (uint8_t *)malloc(TURZX_FRAME_MAX);
	}
	if (ctx->frames[slot] != NULL) {
		memcpy(ctx->frames[slot], data, len);
		ctx->frameLen[slot] = len;
		ctx->head = (ctx->head + 1) % TURZX_QUEUE_SLOTS;
		ctx->count++;
	}
	pthread_mutex_unlock(&ctx->lock);
}

// --- Annex-B assembly ------------------------------------------------------

static const uint8_t kStartCode[4] = {0x00, 0x00, 0x00, 0x01};

// appendParameterSet copies a parameter set with a start code prefix.
static size_t appendParameterSet(uint8_t *dst, size_t cap, size_t off, const uint8_t *nal, size_t len) {
	if (off + 4 + len > cap) return off;
	memcpy(dst + off, kStartCode, 4);
	memcpy(dst + off + 4, nal, len);
	return off + 4 + len;
}

// --- VideoToolbox output ---------------------------------------------------

static void encodeCallback(void *refcon, void *frameRefcon, OSStatus status,
                           VTEncodeInfoFlags infoFlags, CMSampleBufferRef sampleBuffer) {
	TURZXCaptureContext *ctx = (TURZXCaptureContext *)refcon;
	if (ctx == NULL) return;
	if (status != noErr || sampleBuffer == NULL) return;
	if (infoFlags & kVTEncodeInfo_FrameDropped) return;

	CMVideoFormatDescriptionRef fmt = CMSampleBufferGetFormatDescription(sampleBuffer);
	if (fmt == NULL) return;

	uint8_t *buf = ctx->encodeBuf;
	if (buf == NULL) return;
	size_t off = 0;

	// Repeat the parameter sets before every frame. The panel's decoder then
	// needs no cross-frame state, so a stream can be joined at any point
	// (which matters because the first frames may be dropped while the device
	// finishes its streaming prelude).
	for (int i = 0; i < 2; i++) {
		const uint8_t *param = NULL;
		size_t paramLen = 0;
		size_t count = 0;
		if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, i, &param, &paramLen,
		                                                      &count, NULL) != noErr) {
			continue;
		}
		off = appendParameterSet(buf, TURZX_FRAME_MAX, off, param, paramLen);
	}

	CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sampleBuffer);
	if (block == NULL) return;

	size_t totalLen = 0;
	char *dataPtr = NULL;
	if (CMBlockBufferGetDataPointer(block, 0, NULL, &totalLen, &dataPtr) != noErr) return;

	// The encoder emits length-prefixed NAL units; convert to start codes.
	size_t pos = 0;
	while (pos + 4 <= totalLen) {
		uint32_t nalLen = ((uint32_t)(uint8_t)dataPtr[pos] << 24) |
		                  ((uint32_t)(uint8_t)dataPtr[pos + 1] << 16) |
		                  ((uint32_t)(uint8_t)dataPtr[pos + 2] << 8) |
		                  (uint32_t)(uint8_t)dataPtr[pos + 3];
		pos += 4;
		if (nalLen == 0 || pos + nalLen > totalLen) break;
		if (off + 4 + nalLen > TURZX_FRAME_MAX) break;
		memcpy(buf + off, kStartCode, 4);
		memcpy(buf + off + 4, dataPtr + pos, nalLen);
		off += 4 + nalLen;
		pos += nalLen;
	}

	if (off > 0) ctxPush(ctx, buf, off);
}

// --- Rotation ---------------------------------------------------------------

// turzxVImageRotation converts a count of quarter turns clockwise into the
// constant vImage expects.
//
// It cannot simply cast the count: vImage numbers its constants by degrees
// anticlockwise, so kRotate90DegreesClockwise is 3 and kRotate270DegreesClockwise
// is 1. Casting 1 straight through therefore turns the picture the wrong way,
// which is subtle enough to look like a bug in the caller.
static uint8_t turzxVImageRotation(int quarterTurnsClockwise) {
	switch (((quarterTurnsClockwise % 4) + 4) % 4) {
	case 1:  return kRotate90DegreesClockwise;
	case 2:  return kRotate180DegreesClockwise;
	case 3:  return kRotate270DegreesClockwise;
	default: return kRotate0DegreesClockwise;
	}
}

// turzxRotatePixelBuffer returns a new buffer holding src turned clockwise by
// quarterTurns. It uses Accelerate, which does the turn with SIMD and a
// transpose rather than a per-pixel copy.
//
// The caller releases the result. NULL means the rotation failed, in which case
// the caller should encode the original rather than drop the frame.
static CVPixelBufferRef turzxRotatePixelBuffer(CVPixelBufferRef src, int quarterTurns) {
	size_t srcW = CVPixelBufferGetWidth(src);
	size_t srcH = CVPixelBufferGetHeight(src);
	size_t dstW = srcW, dstH = srcH;
	if (quarterTurns == 1 || quarterTurns == 3) {
		dstW = srcH;
		dstH = srcW;
	}

	NSDictionary *attrs = @{ (id)kCVPixelBufferIOSurfacePropertiesKey : @{} };
	CVPixelBufferRef dst = NULL;
	if (CVPixelBufferCreate(kCFAllocatorDefault, dstW, dstH,
	                        kCVPixelFormatType_32BGRA,
	                        (__bridge CFDictionaryRef)attrs, &dst) != kCVReturnSuccess) {
		return NULL;
	}

	CVPixelBufferLockBaseAddress(src, kCVPixelBufferLock_ReadOnly);
	CVPixelBufferLockBaseAddress(dst, 0);

	vImage_Buffer in = {
		.data = CVPixelBufferGetBaseAddress(src),
		.height = srcH,
		.width = srcW,
		.rowBytes = CVPixelBufferGetBytesPerRow(src),
	};
	vImage_Buffer out = {
		.data = CVPixelBufferGetBaseAddress(dst),
		.height = dstH,
		.width = dstW,
		.rowBytes = CVPixelBufferGetBytesPerRow(dst),
	};

	// A quarter turn maps the image exactly, so the background colour is never
	// visible; the API still requires one rather than NULL.
	const Pixel_8 backColor[4] = {0, 0, 0, 0};
	vImage_Error err = vImageRotate90_ARGB8888(&in, &out,
	                                           turzxVImageRotation(quarterTurns),
	                                           backColor, kvImageNoFlags);

	CVPixelBufferUnlockBaseAddress(dst, 0);
	CVPixelBufferUnlockBaseAddress(src, kCVPixelBufferLock_ReadOnly);

	if (err != kvImageNoError) {
		CVPixelBufferRelease(dst);
		return NULL;
	}
	return dst;
}

// --- ScreenCaptureKit output ----------------------------------------------

// TURZXStreamOutput receives captured frames and feeds them to the encoder
// owned by its context.
@interface TURZXStreamOutput : NSObject <SCStreamOutput, SCStreamDelegate>
@property(nonatomic, assign) TURZXCaptureContext *ctx;
@property(nonatomic, assign) VTCompressionSessionRef encoder;
@end

@implementation TURZXStreamOutput

- (void)stream:(SCStream *)stream
    didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
                   ofType:(SCStreamOutputType)type {
	if (type != SCStreamOutputTypeScreen) return;
	if (!CMSampleBufferIsValid(sampleBuffer)) return;
	if (self.encoder == NULL) return;

	CVPixelBufferRef pixelBuffer = CMSampleBufferGetImageBuffer(sampleBuffer);
	if (pixelBuffer == NULL) return;

	// ScreenCaptureKit marks frames that are entirely static; the pixels are
	// still valid, so they are encoded like any other.

	// Rotation has to happen here, before encoding: once a frame is H.264 there
	// are no pixels left to turn.
	uint64_t tA = mach_absolute_time();
	CVPixelBufferRef rotated = NULL;
	if (self.ctx != NULL && self.ctx->quarterTurns != 0) {
		rotated = turzxRotatePixelBuffer(pixelBuffer, self.ctx->quarterTurns);
	}
	if (rotated != NULL) {
		pixelBuffer = rotated;
	}

	uint64_t tB = mach_absolute_time();
	CMTime pts = CMSampleBufferGetPresentationTimeStamp(sampleBuffer);
	VTCompressionSessionEncodeFrame(self.encoder, pixelBuffer, pts, kCMTimeInvalid,
	                                NULL, NULL, NULL);
	uint64_t tC = mach_absolute_time();

	if (rotated != NULL) CVPixelBufferRelease(rotated);
	if (getenv("TURZX_PROF")) {
		static uint64_t rot, enc, n, last;
		static mach_timebase_info_data_t tb;
		if (tb.denom == 0) mach_timebase_info(&tb);
		rot += tB - tA; enc += tC - tB; n++;
		if (n == 60) {
			double k = (double)tb.numer / tb.denom / 1e6 / n;
			double gap = last ? (double)(tA - last) * tb.numer / tb.denom / 1e6 / 60 : 0;
			fprintf(stderr, "prof: rotate=%.1fms encode-submit=%.1fms interval=%.1fms\n", rot * k, enc * k, gap);
			rot = enc = n = 0; last = tA;
		}
	}
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
	if (self.ctx != NULL) self.ctx->lastError = TURZX_E_CAPTURE;
}

@end

// --- C API -----------------------------------------------------------------

// turzx_capture_stop is declared early because start calls it to clean up its
// own partial context on failure.
void turzx_capture_stop(void *handle);

// ensureAppKitInitialised brings up enough of AppKit for ScreenCaptureKit to
// work.
//
// A plain command-line tool has no NSApplication. Without one, building an
// SCContentFilter raises an unrecognized-selector exception deep inside
// CoreFoundation (it messages a private preferences object with a window
// selector), which takes the whole process down rather than returning an error.
// The accessory activation policy keeps this from stealing focus or appearing
// in the Dock.
static void ensureAppKitInitialised(void) {
	static dispatch_once_t once;
	dispatch_once(&once, ^{
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
	});
}

int turzx_capture_start(uint32_t displayID, int width, int height, int encodeWidth, int encodeHeight, int quarterTurns, int fps, int bitrate, void **outHandle) {
	@autoreleasepool {
		if (outHandle) *outHandle = NULL;

		TURZXCaptureContext *ctx = (TURZXCaptureContext *)calloc(1, sizeof(*ctx));
		if (ctx == NULL) return TURZX_E_CAPTURE;
		pthread_mutex_init(&ctx->lock, NULL);
		ctx->lastError = TURZX_E_OK;
		ctx->quarterTurns = quarterTurns;
		ctx->encodeBuf = (uint8_t *)malloc(TURZX_FRAME_MAX);
		if (ctx->encodeBuf == NULL) {
			turzx_capture_stop(ctx);
			return TURZX_E_CAPTURE;
		}

		ensureAppKitInitialised();

		// ScreenCaptureKit does not report a permission failure: it simply
		// returns a display list without the display in it. Check explicitly so
		// the user gets an actionable error instead of a confusing "display not
		// found", and trigger the system prompt on first use.
		if (!CGPreflightScreenCaptureAccess()) {
			CGRequestScreenCaptureAccess();
			turzx_capture_stop(ctx);
			return TURZX_E_PERMISSION;
		}

		// Locate the display in ScreenCaptureKit's own list.
		__block SCDisplay *target = nil;
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		[SCShareableContent getShareableContentWithCompletionHandler:^(
		                        SCShareableContent *content, NSError *error) {
			if (error == nil && content != nil) {
				for (SCDisplay *d in content.displays) {
					if (d.displayID == displayID) {
						target = d;
						break;
					}
				}
			}
			dispatch_semaphore_signal(sem);
		}];
		if (dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC)) != 0) {
			turzx_capture_stop(ctx);
			return TURZX_E_TIMEOUT;
		}
		if (target == nil) {
			// Most often this means Screen Recording permission has not been
			// granted: ScreenCaptureKit then returns a list without the
			// display rather than an explicit error.
			turzx_capture_stop(ctx);
			return TURZX_E_NO_DISPLAY;
		}

		// Build the encoder first so the stream's first frame has somewhere to go.
		NSDictionary *spec = @{ (id)kVTCompressionPropertyKey_RealTime : @YES };
		OSStatus st = VTCompressionSessionCreate(kCFAllocatorDefault,
		                                        (int32_t)encodeWidth, (int32_t)encodeHeight,
		                                        kCMVideoCodecType_H264,
		                                        (__bridge CFDictionaryRef)spec,
		                                        NULL /* sourceImageBufferAttributes */,
		                                        NULL /* compressedDataAllocator */,
		                                        encodeCallback, ctx /* refcon */,
		                                        &ctx->encoder);
		if (st != noErr || ctx->encoder == NULL) {
			turzx_capture_stop(ctx);
			return TURZX_E_ENCODER;
		}

		// Low-latency settings: no frame reordering, no lookahead, and speed
		// preferred over compression efficiency.
		CFBooleanRef yes = kCFBooleanTrue;
		CFBooleanRef no = kCFBooleanFalse;
		int32_t br = (int32_t)bitrate;
		int32_t expectedFPS = (int32_t)fps;
		VTSessionSetProperty(ctx->encoder, kVTCompressionPropertyKey_RealTime, yes);
		VTSessionSetProperty(ctx->encoder, kVTCompressionPropertyKey_AllowFrameReordering, no);
		VTSessionSetProperty(ctx->encoder, kVTCompressionPropertyKey_ProfileLevel,
		                     kVTProfileLevel_H264_High_AutoLevel);
		// Refresh with an IDR at least once a second. A longer interval lets
		// P-frame error accumulate, which shows up as trailing behind moving
		// content -- very visible on a small panel, and worse than the extra
		// bits a shorter GOP costs.
		VTSessionSetProperty(ctx->encoder, kVTCompressionPropertyKey_MaxKeyFrameInterval,
		                     (__bridge CFTypeRef)@(fps));
		VTSessionSetProperty(ctx->encoder, kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration,
		                     (__bridge CFTypeRef)@(1.0));
		VTSessionSetProperty(ctx->encoder, kVTCompressionPropertyKey_ExpectedFrameRate,
		                     (__bridge CFTypeRef)@(expectedFPS));
		VTSessionSetProperty(ctx->encoder, kVTCompressionPropertyKey_AverageBitRate,
		                     (__bridge CFTypeRef)@(br));
		// Allow brief bursts above the average: a desktop frame containing text
		// needs far more bits than a mostly-static one, and capping it to the
		// average is what makes text look soft.
		double bytesPerSecond = (double)br / 8.0;
		NSArray *limits = @[ @(bytesPerSecond * 1.5), @(1.0) ];
		VTSessionSetProperty(ctx->encoder, kVTCompressionPropertyKey_DataRateLimits,
		                     (__bridge CFTypeRef)limits);
		VTCompressionSessionPrepareToEncodeFrames(ctx->encoder);

		SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:target excludingWindows:@[]];
		SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
		cfg.width = (size_t)width;
		cfg.height = (size_t)height;
		cfg.pixelFormat = kCVPixelFormatType_32BGRA;
		cfg.queueDepth = 5;
		// The pointer must be composited into the frames: the panel is a real
		// extended desktop, so without this the cursor vanishes whenever it
		// crosses onto it.
		cfg.showsCursor = YES;
		cfg.minimumFrameInterval = CMTimeMake(1, (int32_t)fps);

		TURZXStreamOutput *output = [[TURZXStreamOutput alloc] init];
		output.ctx = ctx;
		output.encoder = ctx->encoder;
		ctx->output = output;

		// One serial queue per capture keeps a slow panel from stalling another
		// panel's frames; ScreenCaptureKit hands frames to this queue.
		dispatch_queue_t queue = dispatch_queue_create("turzx.capture", DISPATCH_QUEUE_SERIAL);
		ctx->stream = [[SCStream alloc] initWithFilter:filter configuration:cfg delegate:output];
		NSError *addErr = nil;
		if (![ctx->stream addStreamOutput:output
		                              type:SCStreamOutputTypeScreen
		                sampleHandlerQueue:queue
		                             error:&addErr]) {
			turzx_capture_stop(ctx);
			return TURZX_E_CAPTURE;
		}

		dispatch_semaphore_t startSem = dispatch_semaphore_create(0);
		__block int startError = TURZX_E_OK;
		[ctx->stream startCaptureWithCompletionHandler:^(NSError *error) {
			if (error != nil) startError = TURZX_E_CAPTURE;
			dispatch_semaphore_signal(startSem);
		}];
		if (dispatch_semaphore_wait(startSem, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC)) != 0) {
			turzx_capture_stop(ctx);
			return TURZX_E_TIMEOUT;
		}
		if (startError != TURZX_E_OK) {
			turzx_capture_stop(ctx);
			return startError;
		}

		ctx->running = true;
		if (outHandle) *outHandle = ctx;
		return TURZX_E_OK;
	}
}

// turzx_capture_read pops one encoded frame, returning its length, 0 when no
// frame is ready, or -1 on error.
int turzx_capture_read(void *handle, uint8_t *dst, int cap) {
	TURZXCaptureContext *ctx = (TURZXCaptureContext *)handle;
	if (ctx == NULL) return -1;
	if (ctx->lastError != TURZX_E_OK) return -1;

	pthread_mutex_lock(&ctx->lock);
	if (ctx->count == 0) {
		pthread_mutex_unlock(&ctx->lock);
		return 0;
	}
	size_t len = ctx->frameLen[ctx->tail];
	int rc = (int)len;
	if (dst != NULL && len <= (size_t)cap) {
		memcpy(dst, ctx->frames[ctx->tail], len);
	} else {
		rc = -2; // caller's buffer is too small
	}
	ctx->tail = (ctx->tail + 1) % TURZX_QUEUE_SLOTS;
	ctx->count--;
	pthread_mutex_unlock(&ctx->lock);
	return rc;
}

int turzx_capture_error(void *handle) {
	TURZXCaptureContext *ctx = (TURZXCaptureContext *)handle;
	return ctx == NULL ? TURZX_E_CAPTURE : ctx->lastError;
}

void turzx_capture_stop(void *handle) {
	TURZXCaptureContext *ctx = (TURZXCaptureContext *)handle;
	if (ctx == NULL) return;
	@autoreleasepool {
		if (ctx->stream != nil) {
			dispatch_semaphore_t sem = dispatch_semaphore_create(0);
			[ctx->stream stopCaptureWithCompletionHandler:^(NSError *error) {
				dispatch_semaphore_signal(sem);
			}];
			dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC));
			ctx->stream = nil;
		}
		if (ctx->encoder != NULL) {
			VTCompressionSessionCompleteFrames(ctx->encoder, kCMTimeInvalid);
			VTCompressionSessionInvalidate(ctx->encoder);
			CFRelease(ctx->encoder);
			ctx->encoder = NULL;
		}
		// Detach the delegate from the context before the context goes away:
		// the stream may still hold the output object, and a late callback
		// dereferencing a freed context would crash the process.
		if (ctx->output != nil) {
			[(TURZXStreamOutput *)ctx->output setCtx:NULL];
			[(TURZXStreamOutput *)ctx->output setEncoder:NULL];
		}
		ctx->output = nil;
		ctx->running = false;

		pthread_mutex_lock(&ctx->lock);
		ctx->count = 0;
		ctx->head = ctx->tail = 0;
		pthread_mutex_unlock(&ctx->lock);

		for (int i = 0; i < TURZX_QUEUE_SLOTS; i++) {
			if (ctx->frames[i] != NULL) {
				free(ctx->frames[i]);
				ctx->frames[i] = NULL;
			}
		}
		free(ctx->encodeBuf);
		ctx->encodeBuf = NULL;
		pthread_mutex_destroy(&ctx->lock);
		free(ctx);
	}
}

