# dmgbuild settings for the rushbin dmg (macos/build.sh): the app on the left, a link to Applications on the right,
# and the background drawn for these places (rushbin-video, src/Dmg.tsx, at 1× and 2×).
# defines: app (the .app to put in) and background (background.png; its @2x next to it is picked up).
import os.path

app = defines["app"]
files = [app]
symlinks = {"Applications": "/Applications"}
format = "UDZO"
background = defines["background"]
window_rect = ((200, 140), (660, 400))
default_view = "icon-view"
show_status_bar = show_tab_view = show_toolbar = show_pathbar = show_sidebar = False
icon_size = 128
text_size = 13
icon_locations = {os.path.basename(app): (170, 190), "Applications": (490, 190)}
hide_extension = [os.path.basename(app)]
