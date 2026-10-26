// settings_dialog_darwin.m — native Cocoa settings window for macOS.
//
// One window that mirrors the Windows settings page (see
// settings_dialog_darwin.go for the Go side and the rationale): Key field,
// Token field + Regenerate (new token + clipboard + note popup), Copy
// Address row, Language popup that re-localizes every label LIVE, and an
// auto-start checkbox. Replaces the old four-step osascript dialog chain
// where the Regenerate button and the language picker were effectively
// invisible.
//
// Threading: every entry point below is called from the Go main thread
// (the tray menu callback and its nested callbacks all run there), and
// runModalForWindow pumps the modal run loop on that same thread.
//
// Manual reference counting (no ARC under cgo) — but all long-lived views
// are kept in statics and never released individually; they live until
// process exit, which keeps the retain bookkeeping trivial and correct.
#import <Cocoa/Cocoa.h>
#include <stdlib.h>
#include <string.h>
#include "_cgo_export.h"

// Embedded tray artwork (same file the status item uses), defined in
// tray_darwin.m. Returns a NEW image; caller releases.
extern NSImage *samvncTrayIconImage(void);

// Action codes (must match settings_dialog_darwin.go).
enum { kActionRegen = 1, kActionLang = 2, kActionCopy = 3 };

@interface SamVNCSettingsController : NSObject <NSWindowDelegate>
- (void)regenClicked:(id)sender;
- (void)copyClicked:(id)sender;
- (void)langChanged:(id)sender;
- (void)okClicked:(id)sender;
- (void)cancelClicked:(id)sender;
@end

static SamVNCSettingsController *g_ctrl = nil;

// Window + controls (created once, reused for every open).
static NSWindow *g_win = nil;
static NSImageView *g_iconView = nil;
static NSTextField *g_titleLabel = nil;
static NSTextField *g_subtitleLabel = nil;
static NSBox *g_connBox = nil;
static NSBox *g_genBox = nil;
static NSTextField *g_keyLabel = nil;
static NSTextField *g_keyField = nil;
static NSTextField *g_tokenLabel = nil;
static NSTextField *g_tokenField = nil;
static NSTextField *g_tokenHint = nil;
static NSButton *g_btnRegen = nil;
static NSButton *g_btnCopy = nil;
static NSTextField *g_urlLabel = nil;
static NSTextField *g_langLabel = nil;
static NSPopUpButton *g_langPopup = nil;
static NSButton *g_autostartCheck = nil;
static NSButton *g_btnOK = nil;
static NSButton *g_btnCancel = nil;

// Current locale strings kept for actions that need them at click time.
static NSString *g_urlBase = nil;     // public URL WITHOUT token
static NSString *g_copiedPrefix = nil;
static NSString *g_okBtn = nil;

// Result of the last modal session.
static char *g_resKey = nil;
static char *g_resToken = nil;
static int g_resLang = 0;
static int g_resAutostart = 0;
static int g_resOK = 0;

static NSString *nsStr(const char *c) {
    return [NSString stringWithUTF8String:(c ? c : "")];
}

static NSTextField *makeLabel(NSString *text, CGFloat size, BOOL bold,
                              NSColor *color) {
    NSTextField *l = [[NSTextField alloc] initWithFrame:NSMakeRect(0, 0, 10, 10)];
    [l setStringValue:text];
    [l setBezeled:NO];
    [l setDrawsBackground:NO];
    [l setEditable:NO];
    [l setSelectable:NO];
    [l setFont:[NSFont systemFontOfSize:size
                                  weight:bold ? NSFontWeightSemibold
                                              : NSFontWeightRegular]];
    if (color) [l setTextColor:color];
    return l;
}

@implementation SamVNCSettingsController

- (void)regenClicked:(id)sender {
    (void)sender;
    // Go generates the token, fills the field, copies it and shows the
    // note — see traySettingsAction in settings_dialog_darwin.go.
    traySettingsAction(kActionRegen, NULL);
}

- (void)copyClicked:(id)sender {
    (void)sender;
    if (g_urlBase.length == 0) return;
    // Build the URL from the CURRENT token field content so a freshly
    // regenerated (or hand-edited) token is honoured — the Windows dialog
    // historically copied the token captured at open time; this window
    // does it right.
    NSString *tok = [g_tokenField stringValue];
    NSString *url = g_urlBase;
    if (tok.length > 0) {
        NSString *sep = ([url rangeOfString:@"?"].location != NSNotFound) ? @"&" : @"?";
        url = [url stringByAppendingFormat:@"%@token=%@", sep, tok];
    }
    NSPasteboard *pb = [NSPasteboard generalPasteboard];
    [pb clearContents];
    [pb setString:url forType:NSPasteboardTypeString];
    [g_urlLabel setStringValue:[g_copiedPrefix stringByAppendingString:url]];
    traySettingsAction(kActionCopy, NULL);
}

- (void)langChanged:(id)sender {
    (void)sender;
    NSString *code = nil;
    switch ([g_langPopup indexOfSelectedItem]) {
        case 1: code = @"en"; break;
        case 2: code = @"zh"; break;
        default: code = @"auto"; break;
    }
    // Go applies SetLanguage and calls nativeSettingsRelocalize — the
    // labels update inside this click, before the popup even closes.
    traySettingsAction(kActionLang, (char *)[code UTF8String]);
}

- (void)okClicked:(id)sender {
    (void)sender;
    free(g_resKey);
    free(g_resToken);
    g_resKey = strdup([[g_keyField stringValue] UTF8String]);
    g_resToken = strdup([[g_tokenField stringValue] UTF8String]);
    g_resLang = (int)[g_langPopup indexOfSelectedItem];
    g_resAutostart = ([g_autostartCheck state] == NSControlStateValueOn) ? 1 : 0;
    g_resOK = 1;
    [NSApp stopModalWithCode:NSModalResponseOK];
}

- (void)cancelClicked:(id)sender {
    (void)sender;
    g_resOK = 0;
    [NSApp stopModalWithCode:NSModalResponseCancel];
}

- (BOOL)windowShouldClose:(NSWindow *)win {
    (void)win;
    [self cancelClicked:nil];
    return YES;
}

@end

// applyTexts pushes a localized SamVNCSettingsTexts into every control.
// Field values, the popup selection and the checkbox state are untouched —
// this is what makes live language switching possible.
static void applyTexts(const SamVNCSettingsTexts *t) {
    NSString *title = nsStr(t->title);
    NSString *subtitle = nsStr(t->subtitle);
    NSString *groupConn = nsStr(t->groupConn);
    NSString *groupGeneral = nsStr(t->groupGeneral);
    NSString *keyLabel = nsStr(t->keyLabel);
    NSString *keyPh = nsStr(t->keyPlaceholder);
    NSString *tokenLabel = nsStr(t->tokenLabel);
    NSString *tokenHint = nsStr(t->tokenHint);
    NSString *btnRegen = nsStr(t->btnRegen);
    NSString *btnCopy = nsStr(t->btnCopy);
    NSString *copied = nsStr(t->copiedPrefix);
    NSString *langLabel = nsStr(t->langLabel);
    NSString *langAuto = nsStr(t->langAuto);
    NSString *langEn = nsStr(t->langEn);
    NSString *langZh = nsStr(t->langZh);
    NSString *autostart = nsStr(t->autostart);
    NSString *btnOK = nsStr(t->btnOK);
    NSString *btnCancel = nsStr(t->btnCancel);

    if (g_win) [g_win setTitle:title];
    [g_titleLabel setStringValue:title];
    [g_subtitleLabel setStringValue:subtitle];
    [g_connBox setTitle:groupConn];
    [g_genBox setTitle:groupGeneral];
    [g_keyLabel setStringValue:keyLabel];
    [[g_keyField cell] setPlaceholderString:keyPh];
    [g_tokenLabel setStringValue:tokenLabel];
    [g_tokenHint setStringValue:tokenHint];
    [g_btnRegen setTitle:btnRegen];
    [g_btnCopy setTitle:btnCopy];
    [g_btnOK setTitle:btnOK];
    [g_btnCancel setTitle:btnCancel];
    [g_langLabel setStringValue:langLabel];
    [g_autostartCheck setTitle:autostart];

    // Popup items: bilingual constants on purpose (both languages visible
    // so the user can always find theirs) — same as the Windows combo.
    NSArray<NSMenuItem *> *items = [g_langPopup itemArray];
    if (items.count >= 3) {
        [items[0] setTitle:langAuto];
        [items[1] setTitle:langEn];
        [items[2] setTitle:langZh];
    }

    // Copy row: keep whatever feedback state is on screen. First fill
    // (empty label) shows the current address; after a copy click the
    // label already carries the copiedPrefix feedback and is preserved.
    g_copiedPrefix = copied;
    g_okBtn = btnOK;
    // urlBase/urlText arrive EMPTY from relocalize (labels only) — keep
    // the current values in that case, only a real Show replaces them.
    NSString *base = nsStr(t->urlBase);
    if (base.length > 0) g_urlBase = base;
    [g_btnCopy setEnabled:(g_urlBase.length > 0)];
    NSString *cur = [g_urlLabel stringValue];
    if ([cur hasPrefix:copied]) {
        // "copied" feedback on screen — leave it alone.
    } else {
        NSString *display = nsStr(t->urlText);
        if (display.length > 0) [g_urlLabel setStringValue:display];
    }
}

// buildWindow creates the window and all controls once. Frames are manual
// (no autolayout): a fixed 480x412 layout mirrors the Windows dialog.
static void buildWindow(void) {
    if (g_win) return;

    if (!g_ctrl) g_ctrl = [[SamVNCSettingsController alloc] init];

    g_win = [[NSWindow alloc]
        initWithContentRect:NSMakeRect(0, 0, 480, 412)
                  styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable
                    backing:NSBackingStoreBuffered
                      defer:NO];
    [g_win setTitle:@"SamVNC"];
    [g_win setDelegate:g_ctrl];
    [g_win setReleasedWhenClosed:NO];

    NSView *content = [g_win contentView];

    // ---- Header: icon + title + subtitle ----
    g_iconView = [[NSImageView alloc] initWithFrame:NSMakeRect(20, 364, 44, 44)];
    [g_iconView setImageScaling:NSImageScaleProportionallyUpOrDown];
    NSImage *icon = samvncTrayIconImage();
    if (icon) {
        [g_iconView setImage:icon];
        [icon release];
    }
    [content addSubview:g_iconView];

    g_titleLabel = makeLabel(@"SamVNC", 14, YES, nil);
    [g_titleLabel setFrame:NSMakeRect(76, 386, 380, 18)];
    [content addSubview:g_titleLabel];

    g_subtitleLabel = makeLabel(@"", 11, NO,
        [NSColor colorWithCalibratedWhite:0.45 alpha:1.0]);
    [g_subtitleLabel setFrame:NSMakeRect(76, 368, 380, 15)];
    [content addSubview:g_subtitleLabel];

    // ---- Connection box ----
    g_connBox = [[NSBox alloc] initWithFrame:NSMakeRect(20, 178, 440, 182)];
    [g_connBox setTitlePosition:NSAtTop];
    [g_connBox setTitleFont:[NSFont systemFontOfSize:12
                                              weight:NSFontWeightSemibold]];
    NSView *conn = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 416, 146)];
    [g_connBox setContentView:conn];
    [conn release];
    [content addSubview:g_connBox];

    NSView *cv = [g_connBox contentView];

    g_keyLabel = makeLabel(@"", 12, NO, nil);
    [g_keyLabel setFrame:NSMakeRect(12, 116, 118, 16)];
    [g_keyLabel setAlignment:NSTextAlignmentRight];
    [cv addSubview:g_keyLabel];

    g_keyField = [[NSTextField alloc] initWithFrame:NSMakeRect(136, 112, 268, 22)];
    [cv addSubview:g_keyField];

    g_tokenLabel = makeLabel(@"", 12, NO, nil);
    [g_tokenLabel setFrame:NSMakeRect(12, 84, 118, 16)];
    [g_tokenLabel setAlignment:NSTextAlignmentRight];
    [cv addSubview:g_tokenLabel];

    g_tokenField = [[NSTextField alloc] initWithFrame:NSMakeRect(136, 80, 158, 22)];
    [g_tokenField setFont:[NSFont monospacedDigitSystemFontOfSize:11
                                                           weight:NSFontWeightRegular]];
    [cv addSubview:g_tokenField];

    g_btnRegen = [[NSButton alloc]
        initWithFrame:NSMakeRect(302, 78, 102, 26)];
    [g_btnRegen setBezelStyle:NSBezelStyleRounded];
    [g_btnRegen setTarget:g_ctrl];
    [g_btnRegen setAction:@selector(regenClicked:)];
    [cv addSubview:g_btnRegen];

    g_tokenHint = makeLabel(@"", 10, NO,
        [NSColor colorWithCalibratedWhite:0.45 alpha:1.0]);
    [g_tokenHint setFrame:NSMakeRect(136, 48, 268, 28)];
    [g_tokenHint setLineBreakMode:NSLineBreakByWordWrapping];
    [cv addSubview:g_tokenHint];

    g_btnCopy = [[NSButton alloc] initWithFrame:NSMakeRect(12, 8, 102, 26)];
    [g_btnCopy setBezelStyle:NSBezelStyleRounded];
    [g_btnCopy setTarget:g_ctrl];
    [g_btnCopy setAction:@selector(copyClicked:)];
    [cv addSubview:g_btnCopy];

    g_urlLabel = makeLabel(@"", 10, NO, nil);
    [g_urlLabel setFrame:NSMakeRect(124, 6, 280, 30)];
    [g_urlLabel setLineBreakMode:NSLineBreakByCharWrapping];
    // Selectable but not editable: the user can also Cmd+C the URL.
    [g_urlLabel setSelectable:YES];
    [cv addSubview:g_urlLabel];

    // ---- General box ----
    g_genBox = [[NSBox alloc] initWithFrame:NSMakeRect(20, 76, 440, 92)];
    [g_genBox setTitlePosition:NSAtTop];
    [g_genBox setTitleFont:[NSFont systemFontOfSize:12
                                              weight:NSFontWeightSemibold]];
    NSView *gen = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 416, 58)];
    [g_genBox setContentView:gen];
    [gen release];
    [content addSubview:g_genBox];

    NSView *gv = [g_genBox contentView];

    g_langLabel = makeLabel(@"", 12, NO, nil);
    [g_langLabel setFrame:NSMakeRect(12, 30, 118, 16)];
    [g_langLabel setAlignment:NSTextAlignmentRight];
    [gv addSubview:g_langLabel];

    g_langPopup = [[NSPopUpButton alloc] initWithFrame:NSMakeRect(136, 26, 180, 26)];
    [g_langPopup addItemWithTitle:@"Auto"];
    [g_langPopup addItemWithTitle:@"English"];
    [g_langPopup addItemWithTitle:@"中文"];
    [g_langPopup setTarget:g_ctrl];
    [g_langPopup setAction:@selector(langChanged:)];
    [gv addSubview:g_langPopup];

    g_autostartCheck = [[NSButton alloc] initWithFrame:NSMakeRect(136, 4, 260, 18)];
    [g_autostartCheck setButtonType:NSButtonTypeSwitch];
    [g_autostartCheck setTitle:@""];
    [g_autostartCheck setTarget:nil]; // checkbox: no action needed, state read on OK
    [gv addSubview:g_autostartCheck];

    // ---- Bottom buttons ----
    g_btnOK = [[NSButton alloc] initWithFrame:NSMakeRect(392, 24, 68, 28)];
    [g_btnOK setBezelStyle:NSBezelStyleRounded];
    [g_btnOK setKeyEquivalent:@"\r"]; // default button (Enter)
    [g_btnOK setTarget:g_ctrl];
    [g_btnOK setAction:@selector(okClicked:)];
    [content addSubview:g_btnOK];

    g_btnCancel = [[NSButton alloc] initWithFrame:NSMakeRect(316, 24, 68, 28)];
    [g_btnCancel setBezelStyle:NSBezelStyleRounded];
    [g_btnCancel setKeyEquivalent:@"\033"]; // Escape = cancel
    [g_btnCancel setTarget:g_ctrl];
    [g_btnCancel setAction:@selector(cancelClicked:)];
    [content addSubview:g_btnCancel];

    [g_win center];
}

// reactivate re-asserts app activation while the modal session runs — the
// tray app is an accessory (LSUIElement-style) process launched by
// launchd/ssh and macOS 14+ is strict about background activation. The
// timers are added to the MODAL run loop mode (the default mode is not
// serviced while runModalForWindow is active).
static void scheduleReactivate(double delay) {
    NSTimer *t = [NSTimer timerWithTimeInterval:delay
                                        repeats:NO
                                          block:^(NSTimer *tm) {
        (void)tm;
        [NSApp activateIgnoringOtherApps:YES];
    }];
    [[NSRunLoop mainRunLoop] addTimer:t forMode:NSModalPanelRunLoopMode];
}

void nativeSettingsShow(const SamVNCSettingsTexts *t, const char *keyValue,
                        const char *tokenValue, int langIndex,
                        int autostartOn) {
    if (!g_win) {
        // NSWindow creation must happen on the main thread; we are there.
        buildWindow();
    }
    g_resOK = 0;

    applyTexts(t);
    // A real Show always replaces the URL state (relocalize keeps it).
    g_urlBase = nsStr(t->urlBase);
    [g_btnCopy setEnabled:(g_urlBase.length > 0)];
    [g_keyField setStringValue:nsStr(keyValue)];
    [g_tokenField setStringValue:nsStr(tokenValue)];
    [g_langPopup selectItemAtIndex:(NSInteger)langIndex];
    [g_autostartCheck setState:autostartOn ? NSControlStateValueOn
                                           : NSControlStateValueOff];
    [g_urlLabel setStringValue:nsStr(t->urlText)];

    [NSApp activateIgnoringOtherApps:YES];
    scheduleReactivate(0.4);
    scheduleReactivate(1.5);

    [g_win makeKeyAndOrderFront:nil];
    [g_win makeFirstResponder:g_keyField];
    [NSApp runModalForWindow:g_win];
    [g_win orderOut:nil];
}

void nativeSettingsRelocalize(const SamVNCSettingsTexts *t) {
    if (!g_win) return; // nothing on screen — nothing to update
    applyTexts(t);
}

void nativeSettingsSetToken(const char *tok) {
    if (g_tokenField) [g_tokenField setStringValue:nsStr(tok)];
}

void nativeSettingsCopyClipboard(const char *text) {
    NSString *s = nsStr(text);
    if (s.length == 0) return;
    NSPasteboard *pb = [NSPasteboard generalPasteboard];
    [pb clearContents];
    [pb setString:s forType:NSPasteboardTypeString];
}

void nativeSettingsShowNote(const char *msg) {
    if (!g_win) return;
    NSAlert *a = [[NSAlert alloc] init];
    [a setMessageText:nsStr(msg)];
    [a setAlertStyle:NSAlertStyleInformational];
    [a addButtonWithTitle:(g_okBtn ? g_okBtn : @"OK")];
    // Application-modal alert NESTED inside the settings modal session —
    // standard AppKit; it blocks until the user confirms, so Go (which
    // called us from traySettingsAction) stays blocked until dismissal.
    [a runModal];
    [a release];
}

const char *nativeSettingsResultKey(void) {
    return g_resKey ? g_resKey : "";
}

const char *nativeSettingsResultToken(void) {
    return g_resToken ? g_resToken : "";
}

int nativeSettingsResultLang(void) { return g_resLang; }
int nativeSettingsResultAutostart(void) { return g_resAutostart; }
int nativeSettingsResultOK(void) { return g_resOK; }
