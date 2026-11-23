// Virtual display creation on macOS.
//
// macOS has no IddCx equivalent, so there is no supported way for a userspace
// program to add a display. The CGVirtualDisplay classes are private API that
// lives inside the public CoreGraphics framework; they are what DisplayLink,
// BetterDisplay and DeskPad all use. Nothing here is documented by Apple, so
// this could break in any OS update.
//
// The classes are declared as protocols and looked up with NSClassFromString
// rather than being declared as interfaces: they are not exported as linkable
// symbols, so referring to them directly fails at link time. Going through the
// runtime also means a missing class degrades to a nil lookup and a clean error
// instead of a load-time crash.

#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <dispatch/dispatch.h>
#include <math.h>
#include <stdint.h>
#include <stdlib.h>

@protocol TURZXVirtualDisplayMode
- (instancetype)initWithWidth:(NSUInteger)width
                       height:(NSUInteger)height
                  refreshRate:(CGFloat)refreshRate;
@end

@protocol TURZXVirtualDisplayDescriptor
@property(retain) id queue;
@property(retain) id name;
@property unsigned int maxPixelsHigh;
@property unsigned int maxPixelsWide;
@property CGSize sizeInMillimeters;
@property unsigned int serialNum;
@property unsigned int productID;
@property unsigned int vendorID;
- (void)setDispatchQueue:(id)queue;
@end

@protocol TURZXVirtualDisplaySettings
@property(retain) id modes;
@property unsigned int hiDPI;
@end

@protocol TURZXVirtualDisplay
@property(readonly) CGDirectDisplayID displayID;
- (instancetype)initWithDescriptor:(id)descriptor;
- (BOOL)applySettings:(id)settings;
@end

// kLogicalDPI is the pixel density reported for the panel. macOS chooses a
// scaling mode from the ratio of pixel size to physical size, so this decides
// whether the desktop comes out 1:1 or HiDPI-halved. Around 100 DPI yields a
// true 1:1 desktop at the panel's pixel resolution, which is what a 720x1280
// side monitor wants; a realistic 5" physical size would instead be treated as
// a retina panel and give a cramped 360x640 logical area.
static const double kLogicalDPI = 100.0;

// turzx_vd_create adds a virtual display and returns an opaque handle plus its
// CGDirectDisplayID.
//
// Each call creates its own display, so driving two panels means two handles
// and two displays. serialNum distinguishes them in the descriptor: macOS keys
// its remembered arrangement on the identity a descriptor reports, so two
// displays claiming the same serial would trade places whenever either is
// recreated.
int turzx_vd_create(const char *name, int width, int height, int refreshHz,
                    unsigned int serialNum, uint32_t *outID, void **outHandle) {
	@autoreleasepool {
		if (outID) *outID = 0;
		if (outHandle) *outHandle = NULL;
		if (width <= 0 || height <= 0) {
			return -1;
		}

		Class descClass = NSClassFromString(@"CGVirtualDisplayDescriptor");
		Class modeClass = NSClassFromString(@"CGVirtualDisplayMode");
		Class settingsClass = NSClassFromString(@"CGVirtualDisplaySettings");
		Class displayClass = NSClassFromString(@"CGVirtualDisplay");
		if (descClass == nil || modeClass == nil || settingsClass == nil || displayClass == nil) {
			return -4;
		}

		id<TURZXVirtualDisplayDescriptor> desc = [[descClass alloc] init];
		[desc setName:[NSString stringWithUTF8String:name]];
		[desc setDispatchQueue:dispatch_get_main_queue()];
		[desc setVendorID:0x1CBE];
		[desc setProductID:0x0050];
		[desc setSerialNum:serialNum];
		// Leave headroom so macOS can offer scaled modes without rejecting the
		// descriptor outright.
		[desc setMaxPixelsWide:(unsigned int)(width * 2)];
		[desc setMaxPixelsHigh:(unsigned int)(height * 2)];
		[desc setSizeInMillimeters:CGSizeMake(width / kLogicalDPI * 25.4,
		                                      height / kLogicalDPI * 25.4)];

		id<TURZXVirtualDisplay> display = [[displayClass alloc] initWithDescriptor:desc];
		if (display == nil) {
			return -2;
		}

		id<TURZXVirtualDisplayMode> mode = [[modeClass alloc] initWithWidth:(NSUInteger)width
		                                                            height:(NSUInteger)height
		                                                       refreshRate:(CGFloat)refreshHz];
		id<TURZXVirtualDisplaySettings> settings = [[settingsClass alloc] init];
		[settings setHiDPI:0];
		[settings setModes:@[ mode ]];
		if (![display applySettings:settings]) {
			return -3;
		}

		// The display object must outlive this call or macOS tears the display
		// down, so it is stashed behind the handle and released by
		// turzx_vd_destroy. The CFRetain balances the release there: ARC would
		// otherwise drop it at the end of this scope.
		CFRetain((__bridge CFTypeRef)display);
		if (outID) *outID = (uint32_t)[display displayID];
		if (outHandle) *outHandle = (__bridge void *)display;
		return 0;
	}
}

// turzx_vd_destroy releases a display created by turzx_vd_create. macOS removes
// it from the arrangement as the owner lets go.
void turzx_vd_destroy(void *handle) {
	if (handle == NULL) return;
	@autoreleasepool {
		CFRelease((CFTypeRef)handle);
	}
}

