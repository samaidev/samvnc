// tray_darwin.m — Cocoa side of the macOS tray (NSStatusItem + NSMenu).
// Compiled by cgo on darwin only (.m files are darwin-only per go/build).
// Manual reference counting (no ARC under cgo).
#import <Cocoa/Cocoa.h>
#include <stdlib.h>
#include <string.h>
#include "_cgo_export.h"

// NativeMenuItem mirrors the struct in tray_darwin.go.
typedef struct { char* label; char* id; int flags; } NativeMenuItem;

static NSStatusItem *g_statusItem = nil;
static SamVNCHolder *g_holder = nil;

// SamVNCHolder is the menu item target; clicks bounce back into Go.
@interface SamVNCHolder : NSObject
@end
@implementation SamVNCHolder
- (void)itemClicked:(id)sender {
    if ([sender isKindOfClass:[NSMenuItem class]]) {
        trayMenuClickedC((int)[(NSMenuItem *)sender tag]);
    }
}
@end

static void ensureHolder(void) {
    if (!g_holder) {
        g_holder = [[SamVNCHolder alloc] init];
    }
}

static NSImage *defaultIcon(void) {
    NSImage *img = [[NSImage alloc] initWithSize:NSMakeSize(18, 18)];
    [img lockFocus];
    [[NSColor colorWithCalibratedRed:201.0 / 255.0
                               green:100.0 / 255.0
                                blue:66.0 / 255.0
                               alpha:1.0] setFill];
    NSBezierPath *circle =
        [NSBezierPath bezierPathWithOvalInRect:NSMakeRect(1, 1, 16, 16)];
    [circle fill];
    NSDictionary *attrs = @{
        NSFontAttributeName : [NSFont boldSystemFontOfSize:11.0],
        NSForegroundColorAttributeName : [NSColor whiteColor]
    };
    NSAttributedString *s =
        [[NSAttributedString alloc] initWithString:@"S" attributes:attrs];
    NSSize sz = [s size];
    [s drawAtPoint:NSMakePoint((18.0 - sz.width) / 2.0,
                               (18.0 - sz.height) / 2.0)];
    [s release];
    [img unlockFocus];
    return img; // caller releases
}

static void applyIcon(void) {
    ensureHolder();
    if (!g_statusItem) {
        g_statusItem = [[NSStatusBar systemStatusBar]
            statusItemWithLength:NSVariableStatusItemLength];
    }
    if (!g_statusItem.button.image) {
        NSImage *img = defaultIcon();
        img.size = NSMakeSize(18, 18);
        g_statusItem.button.image = img;
        [img release];
    }
}

void nativeTrayRun(void) {
    if (![[NSApp class] respondsToSelector:@selector(activationPolicy)]) {
        // Very old systems: status items still work via legacy API.
    }
    (void)[NSApplication sharedApplication];
    // Accessory policy: no Dock icon, status item remains visible.
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    dispatch_sync(dispatch_get_main_queue(), ^{ applyIcon(); });
    [NSApp run]; // blocks until stop
}

void nativeTrayQuit(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (g_statusItem) {
            [NSStatusBar.systemStatusBar removeStatusItem:g_statusItem];
            g_statusItem = nil;
        }
        [NSApp stop:NSApp];
        // Post a synthetic event so the runloop wakes up and stops.
        NSEvent *e = [NSEvent otherEventWithType:NSEventTypeApplicationDefined
                                        location:NSMakePoint(0, 0)
                                   modifierFlags:0
                                       timestamp:0
                                    windowNumber:0
                                         context:nil
                                         subtype:0
                                           data1:0
                                           data2:0];
        [NSApp postEvent:e atStart:YES];
    });
}

void nativeTraySetTooltip(const char *text) {
    NSString *tip = [NSString stringWithUTF8String:text ? text : ""];
    dispatch_async(dispatch_get_main_queue(), ^{
        if (g_statusItem && g_statusItem.button) {
            g_statusItem.button.toolTip = tip;
        }
    });
}

void nativeTraySetMenu(NativeMenuItem *items, int n) {
    ensureHolder();
    if (n > 0 && !items) return;
    // Copy inputs — the Go side frees its buffers when we return.
    NativeMenuItem *copy = NULL;
    if (n > 0) {
        copy = (NativeMenuItem *)malloc(sizeof(NativeMenuItem) * (size_t)n);
        for (int i = 0; i < n; i++) {
            copy[i].label = strdup(items[i].label ? items[i].label : "");
            copy[i].id = strdup(items[i].id ? items[i].id : "");
            copy[i].flags = items[i].flags;
        }
    }
    dispatch_async(dispatch_get_main_queue(), ^{
        if (!g_statusItem) applyIcon();
        NSMenu *menu = [[NSMenu alloc] init];
        [menu setAutoenablesItems:NO];
        for (int i = 0; i < n; i++) {
            if (copy[i].flags & 4) { // separator
                [menu addItem:[NSMenuItem separatorItem]];
                continue;
            }
            NSMenuItem *it = [[NSMenuItem alloc]
                initWithTitle:[NSString stringWithUTF8String:copy[i].label]
                       action:@selector(itemClicked:)
                keyEquivalent:@""];
            [it setTarget:g_holder];
            [it setTag:i];
            [it setEnabled:!(copy[i].flags & 1)];
            [it setState:(copy[i].flags & 2) ? NSControlStateValueOn
                                             : NSControlStateValueOff];
            [menu addItem:it];
            [it release];
        }
        g_statusItem.menu = menu; // statusItem retains the menu
        [menu release];
        for (int i = 0; i < n; i++) {
            free(copy[i].label);
            free(copy[i].id);
        }
        free(copy);
    });
}
